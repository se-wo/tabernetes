package main

// stress and stress-ng, the load generators of the memory exercises: the
// images polinux/stress, progrium/stress and alexeiled/stress-ng.
//
// The workers are goroutines that never burn the thread: CPU workers do
// ctrAcct.Work (it counts the CPU and sleeps), VM workers charge the memory
// they hold to the container (chargeMem, tabnode_oom.go) and so can pass
// limits.memory. The container is then killed while the worker ramps up, like
// the OOM killer of a real node does. Run in the terminal, the programs have
// no container to charge and only sleep.

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

func init() {
	imagePrograms["stress"] = stressProg
	imagePrograms["stress-ng"] = stressNgProg
	// Appended here, not in the list: the list is only read at run time.
	imageDefs = append(imageDefs,
		&imageDef{
			img:   builtinImage{name: "polinux/stress", cmd: []string{"stress"}, files: alpineFiles},
			repos: []string{"polinux/stress"}, tag: "latest", size: 9_704_386,
		},
		&imageDef{
			img:   builtinImage{name: "progrium/stress", entrypoint: []string{"/usr/bin/stress", "--verbose"}, files: debianFiles},
			repos: []string{"progrium/stress"}, tag: "latest", size: 281_419_803,
		},
		&imageDef{
			img:   builtinImage{name: "alexeiled/stress-ng", entrypoint: []string{"/stress-ng"}, files: alpineFiles},
			repos: []string{"alexeiled/stress-ng"}, tag: "latest", size: 8_113_257,
		},
	)
}

const (
	// vmStep is the size of one allocation of a VM worker: the limit is
	// checked after each one.
	vmStep = 16 << 20
	// vmDefaultBytes is --vm-bytes without a value.
	vmDefaultBytes = 256 << 20
)

// parseStressSize parses a number with an optional suffix of units (the letters of
// units, any case, 1024-based: "b" is 1, "k" is 1<<10, ...).
func parseStressSize(s, units string) (int64, error) {
	n := s
	mult := int64(1)
	if s != "" {
		if i := strings.IndexByte(units, byte(strings.ToLower(s[len(s)-1:])[0])); i >= 0 {
			mult = int64(1) << (10 * i)
			n = s[:len(s)-1]
		}
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid number: %q", s)
	}
	return v * mult, nil
}

// parseSeconds parses a time with an optional suffix s, m, h, d, y (stress)
// or w (stress-ng). A plain number is seconds.
func parseSeconds(s string) (time.Duration, error) {
	mult := map[byte]int64{'s': 1, 'm': 60, 'h': 3600, 'd': 86400, 'w': 7 * 86400, 'y': 365 * 86400}
	n, m := s, int64(1)
	if s != "" {
		if k, ok := mult[s[len(s)-1]]; ok {
			n, m = s[:len(s)-1], k
		}
	}
	v, err := strconv.ParseInt(n, 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("invalid time: %q", s)
	}
	return time.Duration(v*m) * time.Second, nil
}

// hogs is what to run: the workers of stress or stress-ng.
type hogs struct {
	cpu, io, vm, hdd int
	vmBytes          int64
	vmHang           time.Duration
	vmKeep           bool
	timeout          time.Duration
	verbose, quiet   bool
	dry              bool
	backoff          int
}

// stressArgs is the parsed command line of stress.
type stressArgs struct {
	hogs
	help, version bool
}

// stressOpt describes the options of stress: short letter, whether it takes
// a value.
var stressOpts = map[string]struct {
	short string
	value bool
}{
	"help": {"?", false}, "version": {"", false}, "verbose": {"v", false}, "quiet": {"q", false},
	"dry-run": {"n", false}, "timeout": {"t", true}, "backoff": {"", true}, "cpu": {"c", true},
	"io": {"i", true}, "vm": {"m", true}, "vm-bytes": {"", true}, "vm-stride": {"", true},
	"vm-hang": {"", true}, "vm-keep": {"", false}, "hdd": {"d", true}, "hdd-bytes": {"", true},
}

func parseStress(args []string) (*stressArgs, error) {
	a := &stressArgs{}
	a.vmBytes = vmDefaultBytes
	a.backoff = 3000
	set := func(name, val string) error {
		var err error
		count := func(dst *int) {
			var v int64
			v, err = strconv.ParseInt(val, 10, 64)
			if err == nil && v < 0 {
				err = fmt.Errorf("invalid number")
			}
			*dst = int(v)
		}
		switch name {
		case "help":
			a.help = true
		case "version":
			a.version = true
		case "verbose":
			a.verbose = true
		case "quiet":
			a.quiet = true
		case "dry-run":
			a.dry = true
		case "vm-keep":
			a.vmKeep = true
		case "timeout":
			a.timeout, err = parseSeconds(val)
		case "backoff":
			count(&a.backoff)
		case "cpu":
			count(&a.cpu)
		case "io":
			count(&a.io)
		case "vm":
			count(&a.vm)
		case "hdd":
			count(&a.hdd)
		case "vm-bytes":
			a.vmBytes, err = parseStressSize(val, "bkmg")
		case "vm-hang":
			var d time.Duration
			d, err = parseSeconds(val)
			a.vmHang = d
		case "vm-stride", "hdd-bytes":
			_, err = parseStressSize(val, "bkmg")
		}
		if err != nil {
			return fmt.Errorf("invalid argument for --%s: %q", name, val)
		}
		return nil
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case strings.HasPrefix(arg, "--") && len(arg) > 2:
			name, val, hasVal := strings.Cut(arg[2:], "=")
			o, ok := stressOpts[name]
			if !ok {
				return nil, fmt.Errorf("unrecognized option: %s", arg)
			}
			if o.value && !hasVal {
				if i+1 >= len(args) {
					return nil, fmt.Errorf("option requires an argument: %s", arg)
				}
				i++
				val = args[i]
			} else if !o.value && hasVal {
				return nil, fmt.Errorf("option doesn't allow an argument: %s", arg)
			}
			if err := set(name, val); err != nil {
				return nil, err
			}
		case strings.HasPrefix(arg, "-") && len(arg) > 1 && arg != "--":
			for j := 1; j < len(arg); j++ {
				name := ""
				for n, o := range stressOpts {
					if o.short == string(arg[j]) {
						name = n
					}
				}
				if name == "" {
					return nil, fmt.Errorf("unrecognized option: -%c", arg[j])
				}
				val := ""
				if stressOpts[name].value {
					switch {
					case j+1 < len(arg):
						val = arg[j+1:]
					case i+1 < len(args):
						i++
						val = args[i]
					default:
						return nil, fmt.Errorf("option requires an argument: -%c", arg[j])
					}
					j = len(arg)
				}
				if err := set(name, val); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("unrecognized option: %s", arg)
		}
	}
	return a, nil
}

const stressUsage = `'stress' imposes certain types of compute stress on your system

Usage: stress [OPTION [ARG]] ...
 -?, --help         show this help statement
     --version      show version statement
 -v, --verbose      be verbose
 -q, --quiet        be quiet
 -n, --dry-run      show what would have been done
 -t, --timeout N    timeout after N seconds
     --backoff N    wait factor of N microseconds before work starts
 -c, --cpu N        spawn N workers spinning on sqrt()
 -i, --io N         spawn N workers spinning on sync()
 -m, --vm N         spawn N workers spinning on malloc()/free()
     --vm-bytes B   malloc B bytes per vm worker (default is 256MB)
     --vm-stride B  touch a byte every B bytes (default is 4096)
     --vm-hang N    sleep N secs before free (default none, 0 is inf)
     --vm-keep      redirty memory instead of freeing and reallocating
 -d, --hdd N        spawn N workers spinning on write()/unlink()
     --hdd-bytes B  write B bytes per hdd worker (default is 1GB)

Example: stress --cpu 8 --io 4 --vm 2 --vm-bytes 128M --timeout 10s

Note: Numbers may be suffixed with s,m,h,d,y (time) or B,K,M,G (size).
`

func stressProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	a, err := parseStress(args)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "stress: FAIL: [1] (%d) %v\n", 238, err)
		return exitCode(1)
	}
	switch {
	case a.version:
		fmt.Fprintln(hc.Stdout, "stress 1.0.4")
		return nil
	case a.help || a.cpu+a.io+a.vm+a.hdd == 0:
		fmt.Fprint(hc.Stdout, stressUsage)
		return nil
	}
	h := &a.hogs
	say := func(level, format string, v ...any) {
		if level == "dbug" && !h.verbose || level == "info" && h.quiet {
			return
		}
		fmt.Fprintf(hc.Stdout, "stress: %s: [1] %s\n", level, fmt.Sprintf(format, v...))
	}
	say("info", "dispatching hogs: %d cpu, %d io, %d vm, %d hdd", h.cpu, h.io, h.vm, h.hdd)
	say("dbug", "using backoff sleep of %dus", h.backoff*(h.cpu+h.io+h.vm+h.hdd))
	if h.timeout > 0 {
		say("dbug", "setting timeout to %ds", int(h.timeout/time.Second))
	}
	if h.dry {
		return nil
	}
	start := time.Now()
	h.run(ctx, func(worker int, kind string) {
		say("dbug", "--> hog%s worker %d [%d] forked", kind, worker, 6+worker)
	}, func(pid int, format string, v ...any) {
		if h.verbose {
			fmt.Fprintf(hc.Stdout, "stress: dbug: [%d] %s\n", pid, fmt.Sprintf(format, v...))
		}
	})
	if ctx.Err() == nil {
		say("info", "successful run completed in %ds", int(time.Since(start).Seconds()))
	}
	return nil
}

// run starts the workers and waits for them: until the timeout, or until ctx
// ends (the container is killed).
func (h *hogs) run(ctx context.Context, forked func(worker int, kind string), dbug func(pid int, format string, v ...any)) {
	if h.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, h.timeout)
		defer cancel()
	}
	var wg sync.WaitGroup
	start := func(kind string, n int, f func(i int)) {
		for i := 0; i < n; i++ {
			if forked != nil {
				forked(i+1, kind)
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				f(i)
			}()
		}
	}
	start("vm", h.vm, func(i int) { h.vmWorker(ctx, i, 7+i, dbug) })
	start("cpu", h.cpu, func(int) { burn(ctx, 10*time.Millisecond, 0) })
	start("io", h.io, func(int) { burn(ctx, time.Millisecond, time.Millisecond) })
	start("hdd", h.hdd, func(int) { burn(ctx, time.Millisecond, time.Millisecond) })
	wg.Wait()
}

// burn is a worker without memory: CPU-bound work and sleeps, until ctx ends.
func burn(ctx context.Context, work, pause time.Duration) {
	acct := acctFrom(ctx)
	for {
		if acct.Work(ctx, work) != nil {
			return
		}
		if pause > 0 && sleepCtxErr(ctx, pause) != nil {
			return
		}
	}
}

// memTo charges the memory a worker holds to the container of ctx, if any.
func memTo(ctx context.Context, source string, bytes int64) {
	if pe := procOf(ctx); pe != nil && pe.c != nil && pe.proc != nil {
		pe.c.chargeMem(pe.proc, source, bytes)
	}
}

// vmWorker allocates vmBytes in steps and holds them, like the malloc/touch/
// free loop of stress. It checks the limit on the way up.
func (h *hogs) vmWorker(ctx context.Context, n, pid int, dbug func(pid int, format string, v ...any)) {
	source := fmt.Sprintf("stress-vm-%d", n)
	acct := acctFrom(ctx)
	defer memTo(ctx, source, 0)
	for {
		if dbug != nil {
			dbug(pid, "allocating %d bytes ...", h.vmBytes)
		}
		var held int64
		for held < h.vmBytes {
			if acct.Work(ctx, 2*time.Millisecond) != nil {
				return
			}
			held += min(vmStep, h.vmBytes-held)
			memTo(ctx, source, held)
		}
		if dbug != nil {
			dbug(pid, "touching bytes in strides of 4096 bytes ...")
		}
		if h.vmHang > 0 {
			if dbug != nil {
				dbug(pid, "sleeping for %ds with allocated memory", int(h.vmHang/time.Second))
			}
			if sleepCtxErr(ctx, h.vmHang) != nil {
				return
			}
			if !h.vmKeep {
				memTo(ctx, source, 0)
				if dbug != nil {
					dbug(pid, "freed %d bytes", h.vmBytes)
				}
				continue
			}
		}
		// Redirty the memory without end.
		burn(ctx, 10*time.Millisecond, 0)
		return
	}
}

// --- stress-ng ---

const stressNgUsage = `stress-ng, version 0.17.08

Usage: stress-ng [OPTION [ARG]]
 -?, --help                 show help
     --version              show version
 -t N, --timeout N          timeout after N seconds (suffixes s, m, h, d, w, y)
 -c N, --cpu N              start N workers spinning on sqrt(rand())
 -i N, --io N               start N workers spinning on sync()
 -m N, --vm N               start N workers spinning on anonymous mmap
       --vm-bytes N         allocate N bytes per vm worker, or N% of the memory
       --vm-hang N          sleep N seconds before unmapping memory
       --vm-keep            redirty memory instead of unmapping it
       --metrics-brief      show only the metrics of busy stressors
 -v, --verbose              show all debug, warning and normal information output

Example: stress-ng --vm 2 --vm-bytes 128M --timeout 60s
`

// stressNgFlags are the options without a value.
var stressNgFlags = map[string]bool{
	"metrics": true, "metrics-brief": true, "verbose": true, "quiet": true, "vm-keep": true,
	"times": true, "perf": true, "verify": true, "oomable": true, "aggressive": true,
	"help": true, "version": true, "keep-name": true,
}

var stressNgShort = map[string]string{"t": "timeout", "c": "cpu", "i": "io", "m": "vm", "h": "help", "?": "help", "v": "verbose", "q": "quiet", "M": "metrics", "k": "keep-name"}

func stressNgProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	h := &hogs{vmBytes: vmDefaultBytes}
	var help, version bool
	var vmPercent int64
	fail := func(format string, v ...any) error {
		fmt.Fprintf(hc.Stderr, "stress-ng: error: [1] %s\n", fmt.Sprintf(format, v...))
		return exitCode(1)
	}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" {
			return fail("unrecognized option: %s", arg)
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if long, ok := stressNgShort[name]; ok && !strings.HasPrefix(arg, "--") {
			name = long
		}
		if !hasVal && !stressNgFlags[name] {
			// An option with a value, known or not: the next word,
			// unless it is an option itself.
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				val = args[i]
			} else if name == "timeout" || name == "cpu" || name == "io" || name == "vm" || name == "vm-bytes" || name == "vm-hang" {
				return fail("option --%s requires an argument", name)
			}
		}
		var err error
		count := func(dst *int) {
			var v int64
			v, err = strconv.ParseInt(val, 10, 64)
			if err == nil && v < 0 {
				err = fmt.Errorf("negative")
			}
			*dst = int(v)
		}
		switch name {
		case "help":
			help = true
		case "version":
			version = true
		case "vm-keep":
			h.vmKeep = true
		case "timeout":
			h.timeout, err = parseSeconds(val)
		case "cpu":
			count(&h.cpu)
		case "io":
			count(&h.io)
		case "vm":
			count(&h.vm)
		case "vm-hang":
			h.vmHang, err = parseSeconds(val)
		case "vm-bytes":
			if p, ok := strings.CutSuffix(val, "%"); ok {
				vmPercent, err = strconv.ParseInt(p, 10, 64)
				if err == nil && (vmPercent < 1 || vmPercent > 100) {
					err = fmt.Errorf("out of range")
				}
			} else {
				vmPercent = 0
				h.vmBytes, err = parseStressSize(val, "bkmgt")
			}
		}
		if err != nil {
			return fail("invalid argument for --%s: %q", name, val)
		}
	}
	switch {
	case version:
		fmt.Fprintln(hc.Stdout, "stress-ng, version 0.17.08")
		return nil
	case help:
		fmt.Fprint(hc.Stdout, stressNgUsage)
		return nil
	case h.cpu+h.io+h.vm == 0:
		return fail("please specify one or more stressors to run")
	}
	if vmPercent > 0 {
		// of the limit of the container, else of 1 GiB; shared by the workers
		total := int64(1 << 30)
		if pe := procOf(ctx); pe != nil && pe.c != nil {
			if l := memLimit(pe.c.spec); l > 0 {
				total = l
			}
		}
		h.vmBytes = total * vmPercent / 100 / int64(max(h.vm, 1))
	}
	say := func(format string, v ...any) {
		fmt.Fprintf(hc.Stdout, "stress-ng: info:  [1] %s\n", fmt.Sprintf(format, v...))
	}
	if h.timeout > 0 {
		say("setting to a %d secs run per stressor", int(h.timeout/time.Second))
	}
	var list []string
	for _, s := range []struct {
		n    int
		name string
	}{{h.vm, "vm"}, {h.cpu, "cpu"}, {h.io, "io"}} {
		if s.n > 0 {
			list = append(list, fmt.Sprintf("%d %s", s.n, s.name))
		}
	}
	say("dispatching hogs: %s", strings.Join(list, ", "))
	start := time.Now()
	h.run(ctx, nil, nil)
	if ctx.Err() == nil {
		say("successful run completed in %.2f secs", time.Since(start).Seconds())
	}
	return nil
}
