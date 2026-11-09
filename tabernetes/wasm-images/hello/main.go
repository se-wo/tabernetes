// wasi-hello is the demo of WebAssembly pods: a Go program built with
// GOOS=wasip1 GOARCH=wasm. It greets with its args and env, prints the files
// of a mounted directory, ticks with sleeps and exits with a given code.
package main

import (
	"bufio"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

func main() {
	name := flag.String("name", env("NAME", "world"), "who to greet")
	ticks := flag.Int("ticks", 3, "how often to tick, -1 forever")
	interval := flag.Duration("interval", time.Second, "time between ticks")
	dir := flag.String("read", "/config", "print the files of this directory if it exists")
	stdin := flag.Bool("stdin", false, "echo stdin until it ends")
	code := flag.Int("exit", 0, "exit code")
	flag.Parse()

	host, _ := os.Hostname()
	fmt.Printf("Hello, %s! A Go program (%s, %s/%s) in a WebAssembly sandbox.\n", *name, runtime.Version(), runtime.GOOS, runtime.GOARCH)
	fmt.Printf("args: %q\n", os.Args)
	fmt.Printf("hostname: %s, %d environment variables", host, len(os.Environ()))
	for _, k := range []string{"POD_NAME", "GREETING"} {
		if v, ok := os.LookupEnv(k); ok {
			fmt.Printf(", %s=%s", k, v)
		}
	}
	fmt.Println()

	if entries, err := os.ReadDir(*dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			b, err := os.ReadFile(filepath.Join(*dir, e.Name()))
			if err != nil {
				fmt.Fprintf(os.Stderr, "read %s: %v\n", e.Name(), err)
				continue
			}
			fmt.Printf("%s/%s: %s\n", *dir, e.Name(), strings.TrimSpace(string(b)))
		}
	}
	if f, err := os.CreateTemp("", "hello-*"); err == nil {
		fmt.Fprintf(f, "written at %s", time.Now().Format(time.RFC3339))
		f.Close()
		b, _ := os.ReadFile(f.Name())
		fmt.Printf("%s: %s\n", f.Name(), b)
	} else {
		fmt.Fprintf(os.Stderr, "temp file: %v\n", err)
	}

	if *stdin {
		s := bufio.NewScanner(os.Stdin)
		for s.Scan() {
			fmt.Printf("stdin: %s\n", s.Text())
		}
	}
	start := time.Now()
	for i := 1; *ticks < 0 || i <= *ticks; i++ {
		time.Sleep(*interval)
		fmt.Printf("tick %d after %s\n", i, time.Since(start).Round(10*time.Millisecond))
	}
	if *code != 0 {
		fmt.Fprintf(os.Stderr, "exiting with code %d\n", *code)
	}
	os.Exit(*code)
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
