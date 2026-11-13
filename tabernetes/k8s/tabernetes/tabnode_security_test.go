package main

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"mvdan.cc/sh/v3/interp"
)

func secPod(psc *v1.PodSecurityContext, csc *v1.SecurityContext) (*v1.Pod, *v1.Container) {
	pod := &v1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "ns", UID: "uid"}, Spec: v1.PodSpec{SecurityContext: psc}}
	return pod, &v1.Container{Name: "app", SecurityContext: csc}
}

func TestSecResolve(t *testing.T) {
	for _, tc := range []struct {
		name   string
		image  string
		psc    *v1.PodSecurityContext
		csc    *v1.SecurityContext
		uid    int64
		gid    int64
		groups []int64
		home   string
	}{
		{"root", "busybox", nil, nil, 0, 0, []int64{0}, "/root"},
		{"pod user", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}, nil, 1000, 0, []int64{0}, "/"},
		{"container wins", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](3000)},
			&v1.SecurityContext{RunAsUser: ptr.To[int64](2000)}, 2000, 3000, []int64{3000}, "/"},
		{"docs example", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](3000),
			FSGroup: ptr.To[int64](2000), SupplementalGroups: []int64{4000}}, nil, 1000, 3000, []int64{2000, 3000, 4000}, "/"},
		{"nobody", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](65534)}, nil, 65534, 65534, []int64{65534}, "/nonexistent"},
		{"curl image", "curlimages/curl", nil, nil, 100, 101, []int64{101}, "/home/curl_user"},
		{"podinfo image", "ghcr.io/stefanprodan/podinfo", nil, nil, 100, 101, []int64{101}, "/home/app"},
		{"bitnami numeric", "bitnami/kubectl", nil, nil, 1001, 0, []int64{0}, "/"},
		{"hello-app name:group", "us-docker.pkg.dev/google-samples/containers/gke/hello-app:1.0", nil, nil, 65532, 65532, []int64{65532}, "/home/nonroot"},
		{"http-echo uid:gid", "hashicorp/http-echo", nil, nil, 65532, 65532, []int64{65532}, "/"},
		{"runAsUser over image user", "curlimages/curl", nil, &v1.SecurityContext{RunAsUser: ptr.To[int64](1000)}, 1000, 0, []int64{0}, "/"},
		{"runAsGroup only", "curlimages/curl", nil, &v1.SecurityContext{RunAsGroup: ptr.To[int64](5000)}, 100, 5000, []int64{101, 5000}, "/home/curl_user"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, c := secPod(tc.psc, tc.csc)
			img := lookupImage(tc.image)
			s, err := resolveSec(pod, c, img)
			if err != nil {
				t.Fatal(err)
			}
			if s.uid != tc.uid || s.gid != tc.gid || !slices.Equal(s.groups, tc.groups) {
				t.Errorf("uid %d gid %d groups %v, want %d %d %v", s.uid, s.gid, s.groups, tc.uid, tc.gid, tc.groups)
			}
			if h := secHome(pod, c, img); h != tc.home {
				t.Errorf("HOME %s, want %s", h, tc.home)
			}
		})
	}
}

func TestSecRunAsNonRoot(t *testing.T) {
	yes := ptr.To(true)
	for _, tc := range []struct {
		name  string
		image string
		psc   *v1.PodSecurityContext
		csc   *v1.SecurityContext
		want  string
	}{
		{"root image", "nginx", &v1.PodSecurityContext{RunAsNonRoot: yes}, nil,
			`container has runAsNonRoot and image will run as root (pod: "p_ns(uid)", container: app)`},
		{"runAsUser 0", "busybox", nil, &v1.SecurityContext{RunAsNonRoot: yes, RunAsUser: ptr.To[int64](0)},
			`container's runAsUser breaks non-root policy (pod: "p_ns(uid)", container: app)`},
		{"name user", "curlimages/curl", &v1.PodSecurityContext{RunAsNonRoot: yes}, nil,
			`container has runAsNonRoot and image has non-numeric user (curl_user), cannot verify user is non-root (pod: "p_ns(uid)", container: app)`},
		{"name:group user", "hello-app", &v1.PodSecurityContext{RunAsNonRoot: yes}, nil,
			`container has runAsNonRoot and image has non-numeric user (nonroot), cannot verify user is non-root (pod: "p_ns(uid)", container: app)`},
		{"runAsUser 1000", "nginx", &v1.PodSecurityContext{RunAsNonRoot: yes, RunAsUser: ptr.To[int64](1000)}, nil, ""},
		{"numeric image user", "bitnami/kubectl", &v1.PodSecurityContext{RunAsNonRoot: yes}, nil, ""},
		{"container says false", "nginx", &v1.PodSecurityContext{RunAsNonRoot: yes}, &v1.SecurityContext{RunAsNonRoot: ptr.To(false)}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pod, c := secPod(tc.psc, tc.csc)
			_, err := resolveSec(pod, c, lookupImage(tc.image))
			got := ""
			if err != nil {
				got = err.Error()
			}
			if got != tc.want {
				t.Errorf("got %q\nwant %q", got, tc.want)
			}
		})
	}
}

func TestSecCapabilities(t *testing.T) {
	for _, tc := range []struct {
		name     string
		csc      *v1.SecurityContext
		bnd, eff uint64
	}{
		{"default", nil, 0xa80425fb, 0xa80425fb},
		{"drop all add one", &v1.SecurityContext{Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}, Add: []v1.Capability{"NET_BIND_SERVICE"}}}, 0x400, 0x400},
		{"cap prefix", &v1.SecurityContext{Capabilities: &v1.Capabilities{Add: []v1.Capability{"CAP_NET_ADMIN", "sys_time"}, Drop: []v1.Capability{"CHOWN"}}}, (0xa80425fb | 1<<12 | 1<<25) &^ 1, (0xa80425fb | 1<<12 | 1<<25) &^ 1},
		{"add all drop one", &v1.SecurityContext{Capabilities: &v1.Capabilities{Add: []v1.Capability{"ALL"}, Drop: []v1.Capability{"NET_RAW"}}}, 0x1ffffffffff &^ (1 << 13), 0x1ffffffffff &^ (1 << 13)},
		{"privileged", &v1.SecurityContext{Privileged: ptr.To(true), Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}}}, 0x1ffffffffff, 0x1ffffffffff},
		{"non-root", &v1.SecurityContext{RunAsUser: ptr.To[int64](1000)}, 0xa80425fb, 0},
	} {
		pod, c := secPod(nil, tc.csc)
		s, err := resolveSec(pod, c, nil)
		if err != nil {
			t.Fatal(err)
		}
		if s.capBnd != tc.bnd || s.capEff != tc.eff {
			t.Errorf("%s: bnd %016x eff %016x, want %016x %016x", tc.name, s.capBnd, s.capEff, tc.bnd, tc.eff)
		}
	}
}

func TestSecProcStatus(t *testing.T) {
	pod, c := secPod(&v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](3000), FSGroup: ptr.To[int64](2000)},
		&v1.SecurityContext{AllowPrivilegeEscalation: ptr.To(false), SeccompProfile: &v1.SeccompProfile{Type: v1.SeccompProfileTypeRuntimeDefault}})
	s, err := resolveSec(pod, c, nil)
	if err != nil {
		t.Fatal(err)
	}
	st := procStatus("sleep", s)
	for _, want := range []string{
		"Name:\tsleep\n", "Uid:\t1000\t1000\t1000\t1000\n", "Gid:\t3000\t3000\t3000\t3000\n", "Groups:\t2000 3000 \n",
		"CapInh:\t0000000000000000\n", "CapPrm:\t0000000000000000\n", "CapEff:\t0000000000000000\n",
		"CapBnd:\t00000000a80425fb\n", "CapAmb:\t0000000000000000\n", "NoNewPrivs:\t1\n", "Seccomp:\t2\n",
	} {
		if !strings.Contains(st, want) {
			t.Errorf("status lacks %q:\n%s", want, st)
		}
	}
	rp, rc := secPod(nil, nil)
	root, _ := resolveSec(rp, rc, nil)
	if st := procStatus("sh", root); !strings.Contains(st, "CapEff:\t00000000a80425fb\n") || !strings.Contains(st, "NoNewPrivs:\t0\n") {
		t.Errorf("root status:\n%s", st)
	}
}

func TestSecModeAllows(t *testing.T) {
	for _, tc := range []struct {
		name       string
		uid        int64
		groups     []int64
		fuid, fgid int64
		mode       fs.FileMode
		dir        bool
		want       uint32
		dac        bool
		ok         bool
	}{
		{"owner write", 1000, []int64{1000}, 1000, 0, 0o644, false, 2, false, true},
		{"other write", 1000, []int64{1000}, 0, 0, 0o644, false, 2, false, false},
		{"other read", 1000, []int64{1000}, 0, 0, 0o644, false, 4, false, true},
		{"group write", 1000, []int64{0, 2000}, 0, 2000, 0o664, false, 2, false, true},
		{"owner class only", 1000, []int64{1000}, 1000, 1000, 0o077, false, 4, false, false},
		{"search dir", 1000, nil, 0, 0, 0o755, true, 1, false, true},
		{"no search", 1000, nil, 0, 0, 0o700, true, 1, false, false},
		{"create in 1777", 1000, nil, 0, 0, 0o777 | os.ModeSticky, true, 3, false, true},
		{"root with DAC_OVERRIDE", 0, []int64{0}, 1000, 1000, 0o600, false, 6, true, true},
		{"root without DAC_OVERRIDE", 0, []int64{0}, 1000, 1000, 0o600, false, 2, false, false},
		{"root without DAC_OVERRIDE, own file", 0, []int64{0}, 0, 0, 0o644, false, 2, false, true},
		{"DAC_OVERRIDE needs an x bit", 0, []int64{0}, 0, 0, 0o644, false, 1, true, false},
	} {
		if got := modeAllows(tc.uid, tc.groups, tc.fuid, tc.fgid, tc.mode, tc.dir, tc.want, tc.dac); got != tc.ok {
			t.Errorf("%s: %v, want %v", tc.name, got, tc.ok)
		}
	}
	if stickyAllows(1000, 0o777|os.ModeSticky, 0, 2000, false) {
		t.Error("sticky: removed the file of someone else")
	}
	if !stickyAllows(1000, 0o777|os.ModeSticky, 0, 1000, false) || !stickyAllows(0, 0o777|os.ModeSticky, 1, 2, true) || !stickyAllows(1000, 0o777, 0, 2000, false) {
		t.Error("sticky: refused an allowed removal")
	}
}

func TestSecReadOnlyPlace(t *testing.T) {
	r := &rootFS{base: "/x", mounts: []rootMount{{path: "/data/ro", readOnly: true}, {path: "/data"}, {path: "/cfg", readOnly: true}}}
	for _, tc := range []struct {
		p      string
		roRoot bool
		want   bool
	}{
		{"/tmp/x", false, false},
		{"/tmp/x", true, true},
		{"/", true, true},
		{"/data/x", true, false},
		{"/data/ro/x", false, true},
		{"/cfg", false, true},
		{"/cfgx", false, false},
		{"/etc/hosts", true, false},
		{"/etc/passwd", true, true},
		{"/dev/termination-log", true, false},
		{"/dev/shm/x", true, false},
		{"/proc/1/status", true, false},
	} {
		if got := r.readOnlyPlace(tc.p, tc.roRoot); got != tc.want {
			t.Errorf("readOnlyPlace(%s, %v) = %v, want %v", tc.p, tc.roRoot, got, tc.want)
		}
	}
}

func TestSecLsMode(t *testing.T) {
	for m, want := range map[fs.FileMode]string{
		fs.ModeDir | 0o755:                 "drwxr-xr-x",
		fs.ModeDir | 0o777 | fs.ModeSticky: "drwxrwxrwt",
		fs.ModeDir | 0o777 | fs.ModeSetgid: "drwxrwsrwx",
		0o640 | fs.ModeSetgid:              "-rw-r-S---",
		fs.ModeSymlink | 0o777:             "lrwxrwxrwx",
		0o4755&fs.ModePerm | fs.ModeSetuid: "-rwsr-xr-x",
	} {
		if got := lsMode(m); got != want {
			t.Errorf("lsMode(%v) = %s, want %s", m, got, want)
		}
	}
}

// secContainer is a prepared container of a test pod with its root file
// system in a temporary directory.
func secContainer(t *testing.T, image string, psc *v1.PodSecurityContext, csc *v1.SecurityContext, mounts ...rootMount) (*procEnv, *ctr) {
	t.Helper()
	pod, c := secPod(psc, csc)
	c.Image = image
	pod.Spec.Containers = []v1.Container{*c}
	w := testWorker(t, pod.Spec)
	ct := w.ctrs[0]
	ct.image = lookupImage(image)
	sec, err := ct.resolveSec(w.pod)
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	for _, d := range []string{"etc", "tmp", "var/tmp", "root", "proc"} {
		os.MkdirAll(filepath.Join(base, d), 0o755)
	}
	os.WriteFile(filepath.Join(base, "etc/passwd"), []byte(imagePasswd(ct.image)), 0o644)
	os.WriteFile(filepath.Join(base, "etc/group"), []byte(imageGroup(ct.image)), 0o644)
	root := &rootFS{base: base, mounts: mounts}
	prepareRootFS(root, ct.image, sec, []string{"sh"}, "/")
	ct.root, ct.sec, ct.dir = root, sec, "/"
	return &procEnv{w: w, c: ct, root: root, ip: "127.0.0.1"}, ct
}

func runIn(t *testing.T, pe *procEnv, script string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	code := runArgv(context.Background(), pe, []string{"sh", "-c", script}, []string{"PATH=/bin"}, "/", strings.NewReader(""), &out, &out)
	return out.String(), code
}

func TestSecWhoAmI(t *testing.T) {
	for _, tc := range []struct {
		name, image string
		psc         *v1.PodSecurityContext
		want        string
	}{
		{"root", "busybox", nil, "uid=0(root) gid=0(root) groups=0(root)\nroot\n"},
		{"docs example", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), RunAsGroup: ptr.To[int64](3000),
			FSGroup: ptr.To[int64](2000), SupplementalGroups: []int64{4000}}, "uid=1000 gid=3000 groups=2000,3000,4000\nwhoami: unknown uid 1000\n"},
		{"runAsUser only", "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000)}, "uid=1000 gid=0(root) groups=0(root)\nwhoami: unknown uid 1000\n"},
		{"curl", "curlimages/curl", nil, "uid=100(curl_user) gid=101(curl_group) groups=101(curl_group)\ncurl_user\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pe, _ := secContainer(t, tc.image, tc.psc, nil)
			if got, _ := runIn(t, pe, "id; whoami"); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	pe, _ := secContainer(t, "curlimages/curl", nil, nil)
	if got, _ := runIn(t, pe, "id -u; id -g; id -un; id -G; id -Gn"); got != "100\n101\ncurl_user\n101\ncurl_group\n" {
		t.Errorf("id flags: %q", got)
	}
	if got, code := runIn(t, pe, "grep Cap /proc/1/status"); code != 0 || !strings.Contains(got, "CapBnd:\t00000000a80425fb") || !strings.Contains(got, "CapEff:\t0000000000000000") {
		t.Errorf("grep Cap: %d %q", code, got)
	}
	if got := shellPrompt(withProc(context.Background(), pe), "bash", mustRunner(t)); got != "curl_user@p:/$ " {
		t.Errorf("bash prompt %q", got)
	}
	if got := shellPrompt(withProc(context.Background(), pe), "sh", mustRunner(t)); got != "/ $ " {
		t.Errorf("sh prompt %q", got)
	}
}

func mustRunner(t *testing.T) *interp.Runner {
	r, err := newPodRunner(nil, "/", strings.NewReader(""), &bytes.Buffer{}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// The permission checks run on the real file system of the machine: the
// container user is a uid nobody on it has, so the files of the test
// belong to someone else whoever runs it.
func TestSecPermissions(t *testing.T) {
	pe, _ := secContainer(t, "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](4242)}, nil)
	for _, tc := range []struct {
		script, want string
		ok           bool
	}{
		{"echo x > /x", "sh: can't create /x: Permission denied", false},
		{"touch /etc/x", "touch: /etc/x: Permission denied", false},
		{"mkdir /etc/d", "Permission denied", false},
		{"echo x > /tmp/x && cat /tmp/x", "x", true},
		{"mkdir -p /tmp/a && ls /tmp", "a", true},
		{"cat /etc/passwd", "root:x:0:0", true},
		{"[ -w /etc ] || echo no", "no", true},
		{"[ -w /tmp ] && echo yes", "yes", true},
		{"rm /etc/passwd", "Permission denied", false},
	} {
		got, code := runIn(t, pe, tc.script)
		if (code == 0) != tc.ok || !strings.Contains(got, tc.want) {
			t.Errorf("%s: exit %d, %q; want %q (ok %v)", tc.script, code, got, tc.want, tc.ok)
		}
	}
}

func TestSecReadOnlyRoot(t *testing.T) {
	vol := t.TempDir()
	pe, _ := secContainer(t, "busybox", nil, &v1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)},
		rootMount{path: "/data", real: vol}, rootMount{path: "/cfg", real: t.TempDir(), readOnly: true})
	for _, tc := range []struct {
		script, want string
		ok           bool
	}{
		{"echo x > /x", "sh: can't create /x: Read-only file system", false},
		{"touch /tmp/x", "touch: /tmp/x: Read-only file system", false},
		{"mkdir /tmp/d", "Read-only file system", false},
		{"echo x > /data/x && cat /data/x", "x", true},
		{"touch /cfg/x", "Read-only file system", false},
		{"echo 1.2.3.4 x >> /etc/hosts", "", true},
		{"cat /proc/1/status | grep CapEff", "CapEff:\t00000000a80425fb", true},
	} {
		got, code := runIn(t, pe, tc.script)
		if (code == 0) != tc.ok || !strings.Contains(got, tc.want) {
			t.Errorf("%s: exit %d, %q; want %q (ok %v)", tc.script, code, got, tc.want, tc.ok)
		}
	}
}

func TestSecRootWithoutDacOverride(t *testing.T) {
	pe, _ := secContainer(t, "busybox", nil, &v1.SecurityContext{Capabilities: &v1.Capabilities{Drop: []v1.Capability{"ALL"}}})
	dir := pe.root.real("/srv")
	os.MkdirAll(dir, 0o700)
	if err := os.Chown(dir, 4242, 4242); err != nil {
		t.Skipf("chown not permitted here: %v", err)
	}
	if got, code := runIn(t, pe, "touch /srv/x"); code == 0 || !strings.Contains(got, "Permission denied") {
		t.Errorf("root without DAC_OVERRIDE wrote into a 0700 directory of uid 4242: %q", got)
	}
}

func TestSecOwnership(t *testing.T) {
	pe, _ := secContainer(t, "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](1000), FSGroup: ptr.To[int64](2000)}, nil)
	w := pe.w
	w.dir = t.TempDir()
	w.pod.Spec.Volumes = []v1.Volume{{Name: "data", VolumeSource: v1.VolumeSource{EmptyDir: &v1.EmptyDirVolumeSource{}}}}
	vol, _ := w.volumeDir("data")
	os.MkdirAll(vol, 0o755)
	os.WriteFile(filepath.Join(vol, "old"), []byte("x"), 0o600)
	pe.root.mounts = []rootMount{{path: "/data", real: vol}}
	if err := os.Chown(vol, os.Getuid(), 2000); err != nil {
		t.Skipf("chown not permitted here: %v", err)
	}
	w.chownVolumes()
	w.chownVolumes() // twice changes nothing
	fi, _ := os.Stat(vol)
	if lsMode(fi.Mode()) != "drwxrwsrwx" || ownerGID(fi) != 2000 {
		t.Errorf("volume %s gid %d", lsMode(fi.Mode()), ownerGID(fi))
	}
	if fi, _ := os.Stat(filepath.Join(vol, "old")); lsMode(fi.Mode()) != "-rw-rw----" || ownerGID(fi) != 2000 {
		t.Errorf("file in the volume %s gid %d", lsMode(fi.Mode()), ownerGID(fi))
	}
	if got, code := runIn(t, pe, "echo x > /data/f; mkdir /data/d"); code != 0 {
		t.Fatalf("exit %d: %s", code, got)
	}
	for name, dirWant := range map[string]bool{"f": false, "d": true} {
		fi, err := os.Stat(filepath.Join(vol, name))
		if err != nil {
			t.Fatal(err)
		}
		if ownerUID(fi) != 1000 || ownerGID(fi) != 2000 || (fi.Mode()&os.ModeSetgid != 0) != dirWant {
			t.Errorf("%s: %s %d:%d", name, lsMode(fi.Mode()), ownerUID(fi), ownerGID(fi))
		}
	}
	if got, _ := runIn(t, pe, "ls -l /data"); !strings.Contains(got, " 1000     2000    ") {
		t.Errorf("ls -l: %q", got)
	}
	if got, _ := runIn(t, pe, "ls -ld /data"); !strings.HasPrefix(got, "drwxrwsrwx    ") || !strings.Contains(got, " root     2000    ") {
		t.Errorf("ls -ld: %q", got)
	}
}

func TestSecNginxStart(t *testing.T) {
	nginxIn := func(pe *procEnv) (string, error) {
		for p, s := range nginxFiles {
			os.MkdirAll(filepath.Dir(pe.root.real(p)), 0o755)
			os.WriteFile(pe.root.real(p), []byte(s), 0o644)
		}
		var out bytes.Buffer
		err := nginxStartChecks(withProc(context.Background(), pe), interp.HandlerContext{Stderr: &out}, pe.root.real, "/etc/nginx/nginx.conf")
		return out.String(), err
	}
	ro := &v1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)}
	pe, _ := secContainer(t, "nginx", nil, ro)
	if out, err := nginxIn(pe); err == nil || !strings.Contains(out, "nginx: [emerg] mkdir() \"/var/cache/nginx/client_temp\" failed (30: Read-only file system)\n") {
		t.Errorf("read-only root: %v %q", err, out)
	}
	pe, _ = secContainer(t, "nginx", nil, ro, rootMount{path: "/var/cache/nginx", real: t.TempDir()})
	if out, err := nginxIn(pe); err == nil || !strings.Contains(out, "nginx: [emerg] open() \"/run/nginx.pid\" failed (30: Read-only file system)\n") {
		t.Errorf("read-only root with a cache volume: %v %q", err, out)
	}
	pe, _ = secContainer(t, "nginx", nil, ro, rootMount{path: "/var/cache/nginx", real: t.TempDir()}, rootMount{path: "/var/run", real: t.TempDir()})
	if out, err := nginxIn(pe); err != nil {
		t.Errorf("read-only root with both volumes: %v %q", err, out)
	}
	pe, _ = secContainer(t, "nginx", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](4242)}, nil)
	if out, err := nginxIn(pe); err == nil || !strings.Contains(out, "(13: Permission denied)") || !strings.Contains(out, `the "user" directive makes sense only`) {
		t.Errorf("non-root: %v %q", err, out)
	}
	pe, _ = secContainer(t, "nginx", nil, nil)
	if out, err := nginxIn(pe); err != nil || out != "" {
		t.Errorf("root: %v %q", err, out)
	}
}

// The file tools of the shell (coreutils_files.go, sed, tar, dd, awk, jq)
// check like the user of the container, too.
func TestSecFileTools(t *testing.T) {
	pe, _ := secContainer(t, "busybox", &v1.PodSecurityContext{RunAsUser: ptr.To[int64](4242)}, nil)
	os.WriteFile(pe.root.real("/etc/conf"), []byte("a=1\n"), 0o644)
	os.WriteFile(pe.root.real("/tmp/theirs"), []byte("x\n"), 0o644)
	os.WriteFile(pe.root.real("/root/secret"), []byte("x\n"), 0o600)
	// Under node the files are the ones of the machine: without the right
	// to chown, new files do not belong to the user of the container, and
	// the cases that go on with them are skipped.
	probe := filepath.Join(t.TempDir(), "probe")
	os.WriteFile(probe, nil, 0o644)
	canChown := os.Chown(probe, 4242, 4242) == nil
	for _, tc := range []struct {
		script, want string
		ok           bool
		owns         bool // works on entries the container created before
	}{
		{"cp /etc/conf /etc/copy", "Permission denied", false, false},
		{"cp /etc/conf /tmp/copy && cat /tmp/copy", "a=1", true, false},
		{"cp -r /etc /tmp/etc && cat /tmp/etc/conf", "a=1", true, true},
		{"mv /etc/conf /tmp/conf", "Permission denied", false, false},
		{"mv /tmp/theirs /tmp/mine", "Permission denied", false, false}, // the sticky bit
		{"mv /tmp/copy /tmp/copy2 && cat /tmp/copy2", "a=1", true, true},
		{"ln -s /etc/conf /etc/link", "Permission denied", false, false},
		{"ln -s copy2 /tmp/link && cat /tmp/link", "a=1", true, true},
		{"chmod 777 /etc/conf", "Operation not permitted", false, false},
		{"chown 4242 /etc/conf", "Operation not permitted", false, false},
		{"sed -i s/1/2/ /etc/conf", "Permission denied", false, false},
		{"sed s/1/2/ /etc/conf", "a=2", true, false},
		{"dd if=/etc/conf of=/etc/dd status=none", "Permission denied", false, false},
		{"rmdir /etc", "Permission denied", false, false},
		{"mktemp -d /etc/XXXXXX", "Permission denied", false, false},
		{"cd /tmp && tar cf a.tar copy2 && mkdir out && tar xf a.tar -C out && cat out/copy2", "a=1", true, true},
		{"tar xf /tmp/a.tar -C /etc", "Permission denied", false, true},
		{"grep -r x /root", "Permission denied", false, false},
		{"awk '{print > \"/etc/awk\"}' /etc/conf", "", false, false},
		{"find /tmp/copy2 -delete && ls /tmp/copy2", "No such file", false, true},
		{"find /etc/conf -delete", "Permission denied", false, false},
	} {
		if tc.owns && !canChown {
			continue
		}
		got, code := runIn(t, pe, tc.script)
		if (code == 0) != tc.ok || !strings.Contains(got, tc.want) {
			t.Errorf("%s: exit %d, %q; want %q (ok %v)", tc.script, code, got, tc.want, tc.ok)
		}
	}
	if b, _ := os.ReadFile(pe.root.real("/etc/conf")); string(b) != "a=1\n" {
		t.Errorf("/etc/conf changed: %q", b)
	}
}

func TestSecFileToolsReadOnlyRoot(t *testing.T) {
	pe, _ := secContainer(t, "busybox", nil, &v1.SecurityContext{ReadOnlyRootFilesystem: ptr.To(true)}, rootMount{path: "/data", real: t.TempDir()})
	os.WriteFile(pe.root.real("/etc/conf"), []byte("a=1\n"), 0o644)
	for _, tc := range []struct {
		script, want string
		ok           bool
	}{
		{"cp /etc/conf /etc/copy", "Read-only file system", false},
		{"sed -i s/1/2/ /etc/conf", "Read-only file system", false},
		{"chmod 600 /etc/conf", "Read-only file system", false},
		{"mv /etc/conf /data/conf", "Read-only file system", false},
		{"cp /etc/conf /data/conf && sed -i s/1/2/ /data/conf && cat /data/conf", "a=2", true},
		{"cd /data && tar cf a.tar conf && tar xf a.tar -C /tmp", "Read-only file system", false},
		{"mkdir /data/out && tar xf /data/a.tar -C /data/out && cat /data/out/conf", "a=2", true},
	} {
		got, code := runIn(t, pe, tc.script)
		if (code == 0) != tc.ok || !strings.Contains(got, tc.want) {
			t.Errorf("%s: exit %d, %q; want %q (ok %v)", tc.script, code, got, tc.want, tc.ok)
		}
	}
}
