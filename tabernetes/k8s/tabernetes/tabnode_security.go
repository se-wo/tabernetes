package main

// The securityContext of the tab node: who a container runs as, what it may
// do and what it may write. This is emulation, not a sandbox: a container is
// goroutines of the tab, and every check here is a check the commands of the
// tab node make on behalf of the container, so that kubectl exec shows what a
// real cluster shows (id, /proc/1/status, Permission denied, Read-only file
// system, CreateContainerConfigError for runAsNonRoot).
//
// The terminal has no procEnv: it stays root and nothing here applies to it.

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	v1 "k8s.io/api/core/v1"
	"mvdan.cc/sh/v3/interp"
)

// secInfo is the resolved security of a container.
type secInfo struct {
	uid, gid     int64
	groups       []int64 // supplementary groups, the gid included, sorted
	readOnlyRoot bool
	privileged   bool
	noNewPrivs   bool
	seccomp      int    // 0 unconfined, 2 filter (RuntimeDefault, Localhost)
	capBnd       uint64 // bounding set
	capEff       uint64 // permitted and effective set
}

// Linux capability numbers.
var capNames = []string{
	"CHOWN", "DAC_OVERRIDE", "DAC_READ_SEARCH", "FOWNER", "FSETID", "KILL", "SETGID", "SETUID",
	"SETPCAP", "LINUX_IMMUTABLE", "NET_BIND_SERVICE", "NET_BROADCAST", "NET_ADMIN", "NET_RAW",
	"IPC_LOCK", "IPC_OWNER", "SYS_MODULE", "SYS_RAWIO", "SYS_CHROOT", "SYS_PTRACE", "SYS_PACCT",
	"SYS_ADMIN", "SYS_BOOT", "SYS_NICE", "SYS_RESOURCE", "SYS_TIME", "SYS_TTY_CONFIG", "MKNOD",
	"LEASE", "AUDIT_WRITE", "AUDIT_CONTROL", "SETFCAP", "MAC_OVERRIDE", "MAC_ADMIN", "SYSLOG",
	"WAKE_ALARM", "BLOCK_SUSPEND", "AUDIT_READ", "PERFMON", "BPF", "CHECKPOINT_RESTORE",
}

const (
	capChown       = 0
	capDacOverride = 1
	capFowner      = 3
	// capDefault is the default set of containerd: CHOWN, DAC_OVERRIDE,
	// FSETID, FOWNER, MKNOD, NET_RAW, SETGID, SETUID, SETFCAP, SETPCAP,
	// NET_BIND_SERVICE, SYS_CHROOT, KILL, AUDIT_WRITE.
	capDefault uint64 = 0x00000000a80425fb
	capAll     uint64 = 1<<41 - 1
)

// capBit is the bit of a capability name, with or without CAP_; 0 for an
// unknown name.
func capBit(name string) uint64 {
	n := strings.TrimPrefix(strings.ToUpper(strings.TrimSpace(name)), "CAP_")
	if i := slices.Index(capNames, n); i >= 0 {
		return 1 << i
	}
	return 0
}

// capSet applies capabilities.add and drop to the default set the way
// containerd does: add ALL, drop ALL, then the single additions, then the
// single drops. privileged gets every capability.
func capSet(privileged bool, caps *v1.Capabilities) uint64 {
	if privileged {
		return capAll
	}
	set := capDefault
	if caps == nil {
		return set
	}
	isAll := func(c v1.Capability) bool { return strings.EqualFold(string(c), "ALL") }
	if slices.ContainsFunc(caps.Add, isAll) {
		set = capAll
	}
	if slices.ContainsFunc(caps.Drop, isAll) {
		set = 0
	}
	for _, c := range caps.Add {
		if !isAll(c) {
			set |= capBit(string(c))
		}
	}
	for _, c := range caps.Drop {
		if !isAll(c) {
			set &^= capBit(string(c))
		}
	}
	return set
}

func (s *secInfo) hasCap(bit int) bool { return s.capEff&(1<<bit) != 0 }

// --- /etc/passwd and /etc/group ---

type passwdEntry struct {
	name     string
	uid, gid int64
	home     string
}

type groupEntry struct {
	name    string
	gid     int64
	members []string
}

func parsePasswd(s string) []passwdEntry {
	var out []passwdEntry
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(l, ":")
		if len(f) < 7 || strings.HasPrefix(l, "#") {
			continue
		}
		uid, err1 := strconv.ParseInt(f[2], 10, 64)
		gid, err2 := strconv.ParseInt(f[3], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		out = append(out, passwdEntry{name: f[0], uid: uid, gid: gid, home: f[5]})
	}
	return out
}

func parseGroup(s string) []groupEntry {
	var out []groupEntry
	for _, l := range strings.Split(s, "\n") {
		f := strings.Split(l, ":")
		if len(f) < 4 || strings.HasPrefix(l, "#") {
			continue
		}
		gid, err := strconv.ParseInt(f[2], 10, 64)
		if err != nil {
			continue
		}
		g := groupEntry{name: f[0], gid: gid}
		for _, m := range strings.Split(f[3], ",") {
			if m = strings.TrimSpace(m); m != "" {
				g.members = append(g.members, m)
			}
		}
		out = append(out, g)
	}
	return out
}

func passwdByUID(pw []passwdEntry, uid int64) (passwdEntry, bool) {
	for _, e := range pw {
		if e.uid == uid {
			return e, true
		}
	}
	return passwdEntry{}, false
}

func passwdByName(pw []passwdEntry, name string) (passwdEntry, bool) {
	for _, e := range pw {
		if e.name == name {
			return e, true
		}
	}
	return passwdEntry{}, false
}

func groupByGID(gr []groupEntry, gid int64) (groupEntry, bool) {
	for _, g := range gr {
		if g.gid == gid {
			return g, true
		}
	}
	return groupEntry{}, false
}

func groupByName(gr []groupEntry, name string) (groupEntry, bool) {
	for _, g := range gr {
		if g.name == name {
			return g, true
		}
	}
	return groupEntry{}, false
}

// imagePasswd and imageGroup are /etc/passwd and /etc/group of a container:
// the base files of the tab node and the lines of its image.
func imagePasswd(img *builtinImage) string {
	s := "root:x:0:0:root:/root:/bin/sh\nnobody:x:65534:65534:nobody:/nonexistent:/bin/false\n"
	if img != nil {
		for _, l := range img.passwd {
			s += l + "\n"
		}
	}
	return s
}

func imageGroup(img *builtinImage) string {
	s := "root:x:0:\nnobody:x:65534:\n"
	if img != nil {
		for _, l := range img.group {
			s += l + "\n"
		}
	}
	return s
}

// --- resolving the security context ---

// podRef is format.Pod of the kubelet: "name_namespace(uid)".
func podRef(pod *v1.Pod) string {
	return fmt.Sprintf("%s_%s(%s)", pod.Name, pod.Namespace, pod.UID)
}

// verifyRunAsNonRoot is verifyRunAsNonRoot of the kubelet
// (pkg/kubelet/kuberuntime/security_context_others.go). imageUID is nil for
// an image user that is a name.
func verifyRunAsNonRoot(pod *v1.Pod, c *v1.Container, runAsNonRoot *bool, runAsUser, imageUID *int64, imageUser string) error {
	if runAsNonRoot == nil || !*runAsNonRoot {
		return nil
	}
	if runAsUser != nil {
		if *runAsUser == 0 {
			return fmt.Errorf("container's runAsUser breaks non-root policy (pod: %q, container: %s)", podRef(pod), c.Name)
		}
		return nil
	}
	switch {
	case imageUID == nil && imageUser != "":
		return fmt.Errorf("container has runAsNonRoot and image has non-numeric user (%s), cannot verify user is non-root (pod: %q, container: %s)", imageUser, podRef(pod), c.Name)
	case imageUID != nil && *imageUID == 0:
		return fmt.Errorf("container has runAsNonRoot and image will run as root (pod: %q, container: %s)", podRef(pod), c.Name)
	}
	return nil
}

// resolveSec works out the security of a container from the securityContext
// of the pod, the one of the container (it wins for runAsUser, runAsGroup,
// runAsNonRoot and seccompProfile) and the user of the image.
func resolveSec(pod *v1.Pod, c *v1.Container, img *builtinImage) (*secInfo, error) {
	psc := pod.Spec.SecurityContext
	if psc == nil {
		psc = &v1.PodSecurityContext{}
	}
	csc := c.SecurityContext
	if csc == nil {
		csc = &v1.SecurityContext{}
	}
	runAsUser, runAsGroup, runAsNonRoot := psc.RunAsUser, psc.RunAsGroup, psc.RunAsNonRoot
	if csc.RunAsUser != nil {
		runAsUser = csc.RunAsUser
	}
	if csc.RunAsGroup != nil {
		runAsGroup = csc.RunAsGroup
	}
	if csc.RunAsNonRoot != nil {
		runAsNonRoot = csc.RunAsNonRoot
	}

	// The user of the image, like USER of its config: "name", "uid",
	// "name:group" or "uid:gid". The kubelet only looks at the user part.
	imgUser := ""
	if img != nil {
		imgUser = img.user
	}
	userPart, groupPart, _ := strings.Cut(imgUser, ":")
	var imageUID *int64
	if userPart == "" {
		imageUID = new(int64)
	} else if n, err := strconv.ParseInt(userPart, 10, 64); err == nil {
		imageUID = &n
	}
	if err := verifyRunAsNonRoot(pod, c, runAsNonRoot, runAsUser, imageUID, userPart); err != nil {
		return nil, err
	}

	pw, gr := parsePasswd(imagePasswd(img)), parseGroup(imageGroup(img))
	s := &secInfo{}
	switch {
	case runAsUser != nil:
		s.uid = *runAsUser
	case imageUID != nil:
		s.uid = *imageUID
	default:
		e, ok := passwdByName(pw, userPart)
		if !ok {
			return nil, fmt.Errorf("unable to find user %s: no matching entries in passwd file", userPart)
		}
		s.uid = e.uid
	}
	entry, hasEntry := passwdByUID(pw, s.uid)
	switch {
	case runAsGroup != nil:
		s.gid = *runAsGroup
	case runAsUser == nil && groupPart != "":
		if n, err := strconv.ParseInt(groupPart, 10, 64); err == nil {
			s.gid = n
		} else if g, ok := groupByName(gr, groupPart); ok {
			s.gid = g.gid
		} else {
			return nil, fmt.Errorf("unable to find group %s: no matching entries in group file", groupPart)
		}
	case hasEntry:
		s.gid = entry.gid
	}

	groups := []int64{s.gid}
	if psc.FSGroup != nil {
		groups = append(groups, *psc.FSGroup)
	}
	groups = append(groups, psc.SupplementalGroups...)
	if hasEntry {
		for _, g := range gr {
			if slices.Contains(g.members, entry.name) {
				groups = append(groups, g.gid)
			}
		}
	}
	slices.Sort(groups)
	s.groups = slices.Compact(groups)

	s.privileged = csc.Privileged != nil && *csc.Privileged
	s.readOnlyRoot = csc.ReadOnlyRootFilesystem != nil && *csc.ReadOnlyRootFilesystem
	s.noNewPrivs = csc.AllowPrivilegeEscalation != nil && !*csc.AllowPrivilegeEscalation
	seccomp := psc.SeccompProfile
	if csc.SeccompProfile != nil {
		seccomp = csc.SeccompProfile
	}
	if seccomp != nil && (seccomp.Type == v1.SeccompProfileTypeRuntimeDefault || seccomp.Type == v1.SeccompProfileTypeLocalhost) {
		s.seccomp = 2
	}
	s.capBnd = capSet(s.privileged, csc.Capabilities)
	if s.uid == 0 {
		s.capEff = s.capBnd
	}
	return s, nil
}

// resolveSec of a container; nil for WebAssembly pods.
func (c *ctr) resolveSec(pod *v1.Pod) (*secInfo, error) {
	if c.wasm {
		return nil, nil
	}
	return resolveSec(pod, &c.spec, c.image)
}

// secHome is HOME of a container: the home of its user in /etc/passwd, "/"
// for a uid without an entry.
func secHome(pod *v1.Pod, c *v1.Container, img *builtinImage) string {
	if isWasmPod(pod) {
		return "/root"
	}
	s, err := resolveSec(pod, c, img)
	if err != nil || s.uid == 0 {
		return "/root"
	}
	if e, ok := passwdByUID(parsePasswd(imagePasswd(img)), s.uid); ok && e.home != "" {
		return e.home
	}
	return "/"
}

// secOf is the security of the container a command runs in; nil in the
// terminal, in WebAssembly pods and before prepare.
func secOf(ctx context.Context) *secInfo {
	pe := procOf(ctx)
	if pe == nil || pe.c == nil || pe.w == nil {
		return nil
	}
	pe.w.mu.Lock()
	defer pe.w.mu.Unlock()
	return pe.c.sec
}

// --- the root file system ---

// prepareRootFS finishes the root file system of a container for its user:
// /tmp and /var/tmp are 1777, /root is 0700, the home of the image user
// belongs to it,
// /proc/1/status tells who runs, and the working directory exists (owned by
// the user when it is new).
func prepareRootFS(root *rootFS, img *builtinImage, sec *secInfo, argv []string, dir string) {
	for _, d := range []string{"/run", "/var/run"} {
		os.MkdirAll(root.real(d), 0o755)
	}
	for _, d := range []string{"/tmp", "/var/tmp"} {
		os.Chmod(root.real(d), 0o777|os.ModeSticky)
	}
	os.Chmod(root.real("/root"), 0o700)
	if img != nil && img.user != "" {
		user, _, _ := strings.Cut(img.user, ":")
		if e, ok := passwdByName(parsePasswd(imagePasswd(img)), user); ok && strings.HasPrefix(e.home, "/home/") {
			h := root.real(e.home)
			os.MkdirAll(h, 0o755)
			chownIfNeeded(h, e.uid, e.gid)
		}
	}
	wd := root.real(dir)
	_, statErr := os.Stat(wd)
	os.MkdirAll(wd, 0o755)
	if statErr != nil && sec != nil {
		chownIfNeeded(wd, sec.uid, sec.gid)
	}
	if sec != nil {
		name := "sh"
		if len(argv) > 0 {
			name = path.Base(argv[0])
		}
		status := procStatus(name, sec)
		for _, p := range []string{"/proc/1/status", "/proc/self/status"} {
			os.MkdirAll(path.Dir(root.real(p)), 0o755)
			os.WriteFile(root.real(p), []byte(status), 0o444)
		}
	}
}

// procStatus is /proc/<pid>/status of the main process, in the format of
// the kernel.
func procStatus(name string, s *secInfo) string {
	if len(name) > 15 {
		name = name[:15] // TASK_COMM_LEN
	}
	var groups strings.Builder
	for _, g := range s.groups {
		fmt.Fprintf(&groups, "%d ", g)
	}
	nnp := 0
	if s.noNewPrivs {
		nnp = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Name:\t%s\nUmask:\t0022\nState:\tS (sleeping)\nTgid:\t1\nNgid:\t0\nPid:\t1\nPPid:\t0\nTracerPid:\t0\n", name)
	fmt.Fprintf(&b, "Uid:\t%d\t%d\t%d\t%d\n", s.uid, s.uid, s.uid, s.uid)
	fmt.Fprintf(&b, "Gid:\t%d\t%d\t%d\t%d\n", s.gid, s.gid, s.gid, s.gid)
	fmt.Fprintf(&b, "FDSize:\t64\nGroups:\t%s\nNStgid:\t1\nNSpid:\t1\nNSpgid:\t1\nNSsid:\t1\n", groups.String())
	fmt.Fprintf(&b, "Threads:\t1\n")
	fmt.Fprintf(&b, "CapInh:\t%016x\nCapPrm:\t%016x\nCapEff:\t%016x\nCapBnd:\t%016x\nCapAmb:\t%016x\n", 0, s.capEff, s.capEff, s.capBnd, 0)
	fmt.Fprintf(&b, "NoNewPrivs:\t%d\nSeccomp:\t%d\nSeccomp_filters:\t%d\n", nnp, s.seccomp, s.seccomp/2)
	return b.String()
}

// chownIfNeeded gives p to uid:gid unless it already has them.
func chownIfNeeded(p string, uid, gid int64) {
	fi, err := os.Lstat(p)
	if err != nil {
		return
	}
	if ou, og, ok := owner(fi); ok && ou == uid && og == gid {
		return
	}
	os.Lchown(p, int(uid), int(gid))
}

// owner is the owner and group of a file.
func owner(fi fs.FileInfo) (uid, gid int64, ok bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, false
	}
	return int64(st.Uid), int64(st.Gid), true
}

// --- volumes ---

// chownVolumes sets the permissions of the volumes like the kubelet: the
// root of emptyDir, configMap, secret, projected and downwardAPI volumes is
// 0777, and with fsGroup every volume but hostPath belongs to that group
// (SetVolumeOwnership in pkg/volume/volume_linux.go: rw 0660 or ro 0440,
// directories setgid and 0110). It only adds bits, so it is safe to call
// again.
func (w *podWorker) chownVolumes() {
	pod, _ := w.podInfo()
	var fsGroup *int64
	if pod.Spec.SecurityContext != nil {
		fsGroup = pod.Spec.SecurityContext.FSGroup
	}
	for _, v := range pod.Spec.Volumes {
		if v.HostPath != nil {
			continue
		}
		dir, ok := w.volumeDir(v.Name)
		if !ok {
			continue
		}
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		if v.EmptyDir != nil || v.ConfigMap != nil || v.Secret != nil || v.Projected != nil || v.DownwardAPI != nil {
			os.Chmod(dir, fi.Mode()&(os.ModePerm|os.ModeSetgid|os.ModeSetuid|os.ModeSticky)|0o777)
		}
		if fsGroup == nil {
			continue
		}
		readOnly := v.ConfigMap != nil || v.Secret != nil || v.Projected != nil || v.DownwardAPI != nil
		setVolumeOwnership(dir, *fsGroup, readOnly)
	}
}

func setVolumeOwnership(dir string, fsGroup int64, readOnly bool) {
	mask := os.FileMode(0o660)
	if readOnly {
		mask = 0o440
	}
	filepath.Walk(dir, func(p string, fi fs.FileInfo, err error) error {
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		if uid, _, ok := owner(fi); ok {
			chownIfNeeded(p, uid, fsGroup)
		}
		m := mask
		if fi.IsDir() {
			m |= os.ModeSetgid | 0o110
		}
		keep := fi.Mode() & (os.ModePerm | os.ModeSetgid | os.ModeSetuid | os.ModeSticky)
		if keep|m != keep {
			os.Chmod(p, keep|m)
		}
		return nil
	})
}

// --- permission checks ---

type accessKind int

const (
	accRead   accessKind = iota // read a file
	accWrite                    // write a file, or create it when it is missing
	accMkdir                    // create a directory
	accRemove                   // remove an entry
	accList                     // list a directory
	accSearch                   // chdir, search
	accTestR                    // test -r
	accTestW                    // test -w
	accTestX                    // test -x
)

func (k accessKind) writes() bool {
	return k == accWrite || k == accMkdir || k == accRemove || k == accTestW
}

// modeAllows is the classic permission check of Unix: the owner bits for
// the owner, the group bits for a member of the group, the other bits for
// the rest. want is a mask of 4 (r), 2 (w) and 1 (x). dacOverride
// (CAP_DAC_OVERRIDE in the effective set) passes any read and write, any
// search of a directory, and the execution of a file with an x bit.
func modeAllows(uid int64, groups []int64, fileUID, fileGID int64, mode fs.FileMode, isDir bool, want uint32, dacOverride bool) bool {
	perm := uint32(mode.Perm())
	if dacOverride {
		return want&1 == 0 || isDir || perm&0o111 != 0
	}
	var bits uint32
	switch {
	case uid == fileUID:
		bits = perm >> 6
	case slices.Contains(groups, fileGID):
		bits = perm >> 3
	default:
		bits = perm
	}
	return bits&7&want == want
}

// stickyAllows is the rule of the sticky bit: in such a directory only the
// owner of an entry, the owner of the directory or CAP_FOWNER may remove or
// rename the entry.
func stickyAllows(uid int64, dirMode fs.FileMode, dirUID, entryUID int64, fowner bool) bool {
	return dirMode&os.ModeSticky == 0 || fowner || uid == dirUID || uid == entryUID
}

// alwaysWritable are the paths a runtime keeps writable with
// readOnlyRootFilesystem: its own mounts.
func alwaysWritable(p string) bool {
	switch p {
	case "/etc/hosts", "/etc/hostname", "/etc/resolv.conf", "/dev/termination-log", "/proc":
		return true
	}
	return strings.HasPrefix(p, "/dev/") || strings.HasPrefix(p, "/proc/")
}

// readOnlyPlace reports whether the container path p (absolute, clean) is on
// a read-only file system: a readOnly volume mount, or the root file system
// with readOnlyRootFilesystem.
func (r *rootFS) readOnlyPlace(p string, readOnlyRoot bool) bool {
	for _, m := range r.mounts {
		if p == m.path || strings.HasPrefix(p, m.path+"/") || m.path == "/" {
			return m.readOnly
		}
	}
	return readOnlyRoot && !alwaysWritable(p)
}

// containerPath makes p absolute against dir and cleans it.
func containerPath(dir, p string) string {
	if !path.IsAbs(p) {
		p = path.Join(dir, p)
	}
	return path.Clean("/" + p)
}

// fsAccess checks whether the container of ctx may access p (relative to
// dir) the way want says. It returns a *fs.PathError with EROFS or EACCES,
// and nil in the terminal. A path that does not exist passes: the operation
// itself reports ENOENT.
func fsAccess(ctx context.Context, dir, p string, want accessKind) error {
	sec := secOf(ctx)
	pe := procOf(ctx)
	if sec == nil || pe.root == nil {
		return nil
	}
	cp := containerPath(dir, p)
	deny := func(errno syscall.Errno) error { return &fs.PathError{Op: "open", Path: p, Err: errno} }
	if want.writes() && pe.root.readOnlyPlace(cp, sec.readOnlyRoot) {
		// test -w on a missing file is simply false.
		if want != accTestW || exists(pe.root.real(cp)) {
			return deny(syscall.EROFS)
		}
	}
	dac := sec.hasCap(capDacOverride)
	allows := func(fi fs.FileInfo, bits uint32) bool {
		uid, gid, ok := owner(fi)
		if !ok {
			return true
		}
		return modeAllows(sec.uid, sec.groups, uid, gid, fi.Mode(), fi.IsDir(), bits, dac)
	}

	// x on every directory from the root down to the parent.
	parent := path.Dir(cp)
	var walked string
	for _, part := range strings.Split(parent, "/") {
		if part == "" && walked != "" {
			continue
		}
		walked = path.Join("/", walked, part)
		fi, err := os.Stat(pe.root.real(walked))
		if err != nil || !fi.IsDir() {
			return nil
		}
		if !allows(fi, 1) {
			return deny(syscall.EACCES)
		}
	}
	pfi, err := os.Stat(pe.root.real(parent))
	if err != nil {
		return nil
	}
	fi, err := os.Stat(pe.root.real(cp))
	if cp == "/" {
		pfi = fi
	}
	missing := err != nil
	var need uint32
	target := fi
	switch want {
	case accRead, accList, accTestR:
		need = 4
	case accSearch, accTestX:
		need = 1
	case accTestW:
		need = 2
	case accWrite:
		need = 2
		if missing {
			target, need = pfi, 3
		}
	case accMkdir:
		if !missing {
			return nil // EEXIST, from mkdir itself
		}
		target, need = pfi, 3
	case accRemove:
		lfi, err := os.Lstat(pe.root.real(cp))
		if err != nil || cp == "/" {
			return nil
		}
		if !allows(pfi, 3) || !stickyAllows(sec.uid, pfi.Mode(), ownerUID(pfi), ownerUID(lfi), sec.hasCap(capFowner)) {
			return deny(syscall.EACCES)
		}
		return nil
	}
	if target == nil {
		return nil
	}
	if !allows(target, need) {
		return deny(syscall.EACCES)
	}
	return nil
}

func ownerUID(fi fs.FileInfo) int64 { u, _, _ := owner(fi); return u }
func ownerGID(fi fs.FileInfo) int64 { _, g, _ := owner(fi); return g }

func exists(real string) bool {
	_, err := os.Lstat(real)
	return err == nil
}

// fsOwn gives an entry the container just created to the user of the
// container: its uid, and its gid or the one of the parent when the parent
// has the setgid bit (a new directory inherits the bit then).
func fsOwn(ctx context.Context, dir, p string) {
	sec := secOf(ctx)
	pe := procOf(ctx)
	if sec == nil || pe.root == nil {
		return
	}
	cp := containerPath(dir, p)
	real := pe.root.real(cp)
	fi, err := os.Lstat(real)
	if err != nil {
		return
	}
	gid := sec.gid
	if pfi, err := os.Stat(pe.root.real(path.Dir(cp))); err == nil && pfi.Mode()&os.ModeSetgid != 0 {
		gid = ownerGID(pfi)
		if fi.IsDir() && fi.Mode()&os.ModeSetgid == 0 {
			os.Chmod(real, fi.Mode()&(os.ModePerm|os.ModeSticky)|os.ModeSetgid)
		}
	}
	chownIfNeeded(real, sec.uid, gid)
}

// openAs opens a file for a command with the permissions of its container,
// and gives a file it creates to the user of the container.
func openAs(ctx context.Context, hc interp.HandlerContext, name string, flag int, perm os.FileMode) (*os.File, error) {
	return openFileAs(ctx, hc.Dir, name, flag, perm)
}

func openFileAs(ctx context.Context, dir, name string, flag int, perm os.FileMode) (*os.File, error) {
	kind := accRead
	if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE|os.O_TRUNC|os.O_APPEND) != 0 {
		kind = accWrite
	}
	if err := fsAccess(ctx, dir, name, kind); err != nil {
		return nil, err
	}
	real := realPath(ctx, dir, name)
	created := flag&os.O_CREATE != 0 && !exists(real)
	f, err := os.OpenFile(real, flag, perm)
	if err != nil {
		return nil, err
	}
	if created {
		fsOwn(ctx, dir, name)
	}
	return f, nil
}

// podOpenFile is the open handler of the shell of a container for files:
// redirections. A refused open reads like ash: "sh: can't create /x:
// Read-only file system".
func podOpenFile(ctx context.Context, dir, p string, flag int, perm os.FileMode) (io.ReadWriteCloser, error) {
	f, err := openFileAs(ctx, dir, p, flag, perm)
	if err != nil {
		if pe, ok := err.(*fs.PathError); ok && (pe.Err == syscall.EACCES || pe.Err == syscall.EROFS) {
			op := "sh: can't open"
			if flag&(os.O_WRONLY|os.O_RDWR|os.O_CREATE) != 0 {
				op = "sh: can't create"
			}
			return nil, &fs.PathError{Op: op, Path: p, Err: pe.Err}
		}
		return nil, err
	}
	return f, nil
}

// readFileAs reads a whole file with the permissions of the container.
func readFileAs(ctx context.Context, dir, name string) ([]byte, error) {
	if err := fsAccess(ctx, dir, name, accRead); err != nil {
		return nil, err
	}
	return os.ReadFile(realPath(ctx, dir, name))
}

// readDirAs lists a directory with the permissions of the container.
func readDirAs(ctx context.Context, hc interp.HandlerContext, name string) ([]fs.DirEntry, error) {
	if err := fsAccess(ctx, hc.Dir, name, accList); err != nil {
		return nil, err
	}
	return os.ReadDir(abs(ctx, hc, name))
}

// mkdirAs is mkdir [-p] with the permissions of the container. New
// directories belong to its user.
func mkdirAs(ctx context.Context, hc interp.HandlerContext, name string, parents bool) error {
	if !parents {
		if err := fsAccess(ctx, hc.Dir, name, accMkdir); err != nil {
			return err
		}
		if err := os.Mkdir(abs(ctx, hc, name), 0o755); err != nil {
			return err
		}
		fsOwn(ctx, hc.Dir, name)
		return nil
	}
	// -p: one directory after the other, so each is checked and owned.
	cp := containerPath(hc.Dir, name)
	walked := ""
	for _, part := range strings.Split(strings.TrimPrefix(cp, "/"), "/") {
		if part == "" {
			continue
		}
		walked += "/" + part
		real := realPath(ctx, "/", walked)
		if fi, err := os.Stat(real); err == nil {
			if !fi.IsDir() {
				return &fs.PathError{Op: "mkdir", Path: name, Err: syscall.ENOTDIR}
			}
			continue
		}
		if err := fsAccess(ctx, "/", walked, accMkdir); err != nil {
			return &fs.PathError{Op: "mkdir", Path: name, Err: unwrap(err)}
		}
		if err := os.Mkdir(real, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
		fsOwn(ctx, "/", walked)
	}
	return nil
}

// removeAs is rm [-r] with the permissions of the container: w and x on
// the directory of every entry, and the sticky bit.
func removeAs(ctx context.Context, hc interp.HandlerContext, name string, recursive bool) error {
	if err := fsAccess(ctx, hc.Dir, name, accRemove); err != nil {
		return err
	}
	real := abs(ctx, hc, name)
	if !recursive {
		return os.Remove(real)
	}
	if secOf(ctx) != nil {
		cp := containerPath(hc.Dir, name)
		var denied error
		filepath.WalkDir(real, func(p string, d fs.DirEntry, err error) error {
			if err != nil || denied != nil || p == real {
				return nil
			}
			rel, _ := filepath.Rel(real, p)
			if err := fsAccess(ctx, "/", path.Join(cp, filepath.ToSlash(rel)), accRemove); err != nil {
				denied = err
				return filepath.SkipAll
			}
			return nil
		})
		if denied != nil {
			return denied
		}
	}
	return os.RemoveAll(real)
}

// writeFileAs replaces the content of a file with the permissions of the
// container.
func writeFileAs(ctx context.Context, hc interp.HandlerContext, name string, data []byte, perm os.FileMode) error {
	f, err := openAs(ctx, hc, name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// createAs checks that the container may create the entry name (a link, a
// directory, the target of a rename): w and x on its directory. An entry
// that is already there must be removable.
func createAs(ctx context.Context, hc interp.HandlerContext, name string) error {
	if err := fsAccess(ctx, hc.Dir, name, accRemove); err != nil {
		return err
	}
	return fsAccess(ctx, hc.Dir, name, accMkdir)
}

// removeCheck checks that the container may remove or rename the entry name.
func removeCheck(ctx context.Context, hc interp.HandlerContext, name string) error {
	return fsAccess(ctx, hc.Dir, name, accRemove)
}

// attrCheck is the rule of chmod and chown: the file system is writable,
// and the user owns the file or has the capability (CAP_FOWNER for chmod,
// CAP_CHOWN for chown). A user without CAP_CHOWN may still give its own
// file to one of its groups.
func attrCheck(ctx context.Context, hc interp.HandlerContext, name string, capBit int, uid, gid int) error {
	sec, pe := secOf(ctx), procOf(ctx)
	if sec == nil || pe.root == nil {
		return nil
	}
	cp := containerPath(hc.Dir, name)
	deny := func(errno syscall.Errno) error { return &fs.PathError{Op: "chmod", Path: name, Err: errno} }
	if pe.root.readOnlyPlace(cp, sec.readOnlyRoot) {
		return deny(syscall.EROFS)
	}
	if sec.hasCap(capBit) {
		return nil
	}
	fi, err := os.Lstat(pe.root.real(cp))
	if err != nil {
		return nil // the operation itself reports it
	}
	fuid, fgid, ok := owner(fi)
	if !ok {
		return nil
	}
	if fuid != sec.uid {
		return deny(syscall.EPERM)
	}
	if capBit == capChown {
		sameUser := uid < 0 || int64(uid) == fuid
		ownGroup := gid < 0 || int64(gid) == fgid || slices.Contains(sec.groups, int64(gid))
		if !sameUser || !ownGroup {
			return deny(syscall.EPERM)
		}
	}
	return nil
}

// podAccess is the access handler of the shell of a container: test -r,
// -w, -x and cd.
func podAccess(ctx context.Context, dir, p string, mode interp.AccessMode) error {
	if _, err := os.Stat(realPath(ctx, dir, p)); err != nil {
		return err
	}
	for bit, kind := range map[interp.AccessMode]accessKind{interp.AccessRead: accTestR, interp.AccessWrite: accTestW, interp.AccessExec: accTestX} {
		if mode&bit != 0 {
			if err := fsAccess(ctx, dir, p, kind); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- who am I ---

// userDB is /etc/passwd and /etc/group of the container of ctx.
func userDB(ctx context.Context) ([]passwdEntry, []groupEntry) {
	pe := procOf(ctx)
	if pe == nil || pe.root == nil {
		return parsePasswd(imagePasswd(nil)), parseGroup(imageGroup(nil))
	}
	pw, _ := os.ReadFile(pe.root.real("/etc/passwd"))
	gr, _ := os.ReadFile(pe.root.real("/etc/group"))
	return parsePasswd(string(pw)), parseGroup(string(gr))
}

// whoIs is the uid, gid and groups of the container of ctx: root in the
// terminal.
func whoIs(ctx context.Context) *secInfo {
	if s := secOf(ctx); s != nil {
		return s
	}
	return &secInfo{groups: []int64{0}, capBnd: capDefault, capEff: capDefault}
}

func userName(pw []passwdEntry, uid int64) (string, bool) {
	if e, ok := passwdByUID(pw, uid); ok {
		return e.name, true
	}
	return strconv.FormatInt(uid, 10), false
}

func groupName(gr []groupEntry, gid int64) (string, bool) {
	if g, ok := groupByGID(gr, gid); ok {
		return g.name, true
	}
	return strconv.FormatInt(gid, 10), false
}

// idCmd is id [-u|-g|-G] [-n] [-r] [user], like BusyBox.
func idCmd(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	o, args, err := opts("id", args, "ugGnr", map[string]string{"user": "u", "group": "g", "groups": "G", "name": "n", "real": "r"})
	if err != nil {
		return fail(hc, 1, "%v", err)
	}
	pw, gr := userDB(ctx)
	s := whoIs(ctx)
	uid, gid, groups := s.uid, s.gid, s.groups
	if len(args) > 0 {
		e, ok := passwdByName(pw, args[0])
		if !ok {
			if n, err := strconv.ParseInt(args[0], 10, 64); err == nil {
				e, ok = passwdByUID(pw, n)
			}
		}
		if !ok {
			return fail(hc, 1, "id: unknown user %s", args[0])
		}
		uid, gid, groups = e.uid, e.gid, []int64{e.gid}
		for _, g := range gr {
			if slices.Contains(g.members, e.name) {
				groups = append(groups, g.gid)
			}
		}
		slices.Sort(groups)
		groups = slices.Compact(groups)
	}
	name := has(o, "n")
	uname := func(id int64) string {
		if n, ok := userName(pw, id); ok && name {
			return n
		}
		return strconv.FormatInt(id, 10)
	}
	gname := func(id int64) string {
		if n, ok := groupName(gr, id); ok && name {
			return n
		}
		return strconv.FormatInt(id, 10)
	}
	switch {
	case has(o, "u"):
		fmt.Fprintln(hc.Stdout, uname(uid))
	case has(o, "g"):
		fmt.Fprintln(hc.Stdout, gname(gid))
	case has(o, "G"):
		var l []string
		for _, g := range groups {
			l = append(l, gname(g))
		}
		fmt.Fprintln(hc.Stdout, strings.Join(l, " "))
	default:
		withName := func(id int64, n string, ok bool) string {
			if ok {
				return fmt.Sprintf("%d(%s)", id, n)
			}
			return strconv.FormatInt(id, 10)
		}
		un, uok := userName(pw, uid)
		gn, gok := groupName(gr, gid)
		var l []string
		for _, g := range groups {
			n, ok := groupName(gr, g)
			l = append(l, withName(g, n, ok))
		}
		fmt.Fprintf(hc.Stdout, "uid=%s gid=%s groups=%s\n", withName(uid, un, uok), withName(gid, gn, gok), strings.Join(l, ","))
	}
	return nil
}

func whoamiCmd(ctx context.Context, hc interp.HandlerContext, _ []string, _ interp.ExecHandlerFunc) error {
	pw, _ := userDB(ctx)
	uid := whoIs(ctx).uid
	n, ok := userName(pw, uid)
	if !ok {
		return fail(hc, 1, "whoami: unknown uid %d", uid)
	}
	fmt.Fprintln(hc.Stdout, n)
	return nil
}

// procUser is the USER column of ps: the name, or the uid without one.
func procUser(ctx context.Context) string {
	pw, _ := userDB(ctx)
	n, _ := userName(pw, whoIs(ctx).uid)
	return n
}

// promptUser is the user and the sign of a shell prompt: "root" and "#", a
// name and "$", or "I have no name!" like bash for a uid without an entry.
func promptUser(ctx context.Context) (user, sign string) {
	s := secOf(ctx)
	if s == nil || s.uid == 0 {
		return "root", "#"
	}
	pw, _ := userDB(ctx)
	n, ok := userName(pw, s.uid)
	if !ok {
		n = "I have no name!"
	}
	return n, "$"
}

// lsOwner is the owner and group of ls -l: names from the container's
// /etc/passwd and /etc/group, numbers without; "root root" in the terminal.
func lsOwner(ctx context.Context, fi fs.FileInfo) string {
	pe := procOf(ctx)
	uid, gid, ok := owner(fi)
	if pe == nil || pe.root == nil || !ok {
		return "root root"
	}
	pw, gr := userDB(ctx)
	u, _ := userName(pw, uid)
	g, _ := groupName(gr, gid)
	return u + " " + g
}

// lsMode is the mode of ls -l: "drwxrwsrwx", "drwxrwxrwt".
func lsMode(m fs.FileMode) string {
	b := []byte("-rwxrwxrwx")
	switch {
	case m&fs.ModeDir != 0:
		b[0] = 'd'
	case m&fs.ModeSymlink != 0:
		b[0] = 'l'
	case m&fs.ModeNamedPipe != 0:
		b[0] = 'p'
	case m&fs.ModeSocket != 0:
		b[0] = 's'
	case m&fs.ModeCharDevice != 0:
		b[0] = 'c'
	case m&fs.ModeDevice != 0:
		b[0] = 'b'
	}
	for i := 0; i < 9; i++ {
		if m&(1<<(8-i)) == 0 {
			b[i+1] = '-'
		}
	}
	special := func(set bool, i int, lower, upper byte) {
		if !set {
			return
		}
		if b[i] == 'x' {
			b[i] = lower
		} else {
			b[i] = upper
		}
	}
	special(m&fs.ModeSetuid != 0, 3, 's', 'S')
	special(m&fs.ModeSetgid != 0, 6, 's', 'S')
	special(m&fs.ModeSticky != 0, 9, 't', 'T')
	return string(b)
}

// --- nginx ---

var (
	ngxPidRe  = regexp.MustCompile(`(?m)^\s*pid\s+([^;\s]+)\s*;`)
	ngxUserRe = regexp.MustCompile(`^\s*user\s+[^;]*;`)
)

// nginxStartChecks are the start of nginx that a securityContext breaks:
// the temporary directories in /var/cache/nginx and the pid file. On
// failure it logs like nginx and returns exit status 1.
func nginxStartChecks(ctx context.Context, hc interp.HandlerContext, real func(string) string, file string) error {
	s := secOf(ctx)
	if s == nil {
		return nil
	}
	nowLog := func() string { return time.Now().UTC().Format("2006/01/02 15:04:05") }
	conf, _ := os.ReadFile(real(file))
	emerg := func(format string, a ...any) error {
		msg := fmt.Sprintf(format, a...)
		fmt.Fprintf(hc.Stderr, "%s [emerg] 1#1: %s\nnginx: [emerg] %s\n", nowLog(), msg, msg)
		return interp.ExitStatus(1)
	}
	errnoText := func(err error) string {
		if e, ok := unwrap(err).(syscall.Errno); ok {
			return fmt.Sprintf("%d: %v", int(e), e)
		}
		return unwrap(err).Error()
	}
	if s.uid != 0 {
		for i, l := range strings.Split(string(conf), "\n") {
			if ngxUserRe.MatchString(l) {
				msg := fmt.Sprintf("the \"user\" directive makes sense only if the master process runs with super-user privileges, ignored in %s:%d", file, i+1)
				fmt.Fprintf(hc.Stderr, "%s [warn] 1#1: %s\nnginx: [warn] %s\n", nowLog(), msg, msg)
				break
			}
		}
	}
	if fi, err := os.Stat(real("/var/cache/nginx")); err == nil && fi.IsDir() {
		for _, t := range []string{"client_temp", "proxy_temp", "fastcgi_temp", "uwsgi_temp", "scgi_temp"} {
			p := "/var/cache/nginx/" + t
			if exists(real(p)) {
				continue
			}
			if err := fsAccess(ctx, "/", p, accMkdir); err != nil {
				return emerg("mkdir() \"%s\" failed (%s)", p, errnoText(err))
			}
			if os.Mkdir(real(p), 0o700) == nil {
				fsOwn(ctx, "/", p)
			}
		}
	}
	pid := "/run/nginx.pid"
	if m := ngxPidRe.FindSubmatch(conf); m != nil {
		pid = string(m[1])
	}
	// /var/run is a link to /run in the image: an emptyDir on either works.
	candidates := []string{pid}
	if rest, ok := strings.CutPrefix(pid, "/run/"); ok {
		candidates = append(candidates, "/var/run/"+rest)
	} else if rest, ok := strings.CutPrefix(pid, "/var/run/"); ok {
		candidates = append(candidates, "/run/"+rest)
	}
	var first error
	for _, p := range candidates {
		err := fsAccess(ctx, "/", p, accWrite)
		if err == nil {
			if f, err := openFileAs(ctx, "/", p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
				f.WriteString("1\n")
				f.Close()
			}
			return nil
		}
		if first == nil {
			first = err
		}
	}
	return emerg("open() \"%s\" failed (%s)", pid, errnoText(first))
}
