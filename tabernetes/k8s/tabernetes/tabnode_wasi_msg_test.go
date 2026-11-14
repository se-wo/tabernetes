package main

import (
	"bytes"
	"strconv"
	"strings"
	"syscall/js"
	"testing"
)

// testPeer gives a peer whose posts go into a list, and handlers of the test
// in place of the registry.
func testPeer(t *testing.T, handlers map[string]wasiHandler, closers map[string][]func(*wasiPeer, uint32)) (*wasiPeer, *[]js.Value) {
	t.Helper()
	savedH, savedC := wasiHandlers, wasiClosers
	wasiHandlers, wasiClosers = handlers, closers
	if wasiHandlers == nil {
		wasiHandlers = map[string]wasiHandler{}
	}
	if wasiClosers == nil {
		wasiClosers = map[string][]func(*wasiPeer, uint32){}
	}
	t.Cleanup(func() { wasiHandlers, wasiClosers = savedH, savedC })
	var posted []js.Value
	return newWasiPeer("ns/pod/c", func(msg, _ js.Value) { posted = append(posted, msg) }), &posted
}

func buf(n int) js.Value { return js.Global().Get("ArrayBuffer").New(n) }

func msg(fields map[string]any) js.Value {
	o := js.Global().Get("Object").New()
	for k, v := range fields {
		o.Set(k, v)
	}
	return o
}

func TestWasiMessagesParse(t *testing.T) {
	if len(wasiSpecs) != len(wasiMessages) {
		t.Fatalf("%d specs for %d messages", len(wasiSpecs), len(wasiMessages))
	}
	// Each message of section 8.3 and the CLI messages are in the table.
	for _, typ := range []string{"bind", "bound", "listen", "listening", "accepted", "connect", "connected", "data", "datagram",
		"credit", "error", "shutdown", "closed", "lookup", "addresses", "request", "response", "cancel", "fetch", "fetched",
		"fetch-cancel", "cpu", "start", "ready", "exit", "stdout", "stderr", "stdin", "mem"} {
		if wasiSpecs[typ] == nil {
			t.Errorf("message %s is not in the table", typ)
		}
	}
	for typ, s := range wasiSpecs {
		for _, f := range s.fields {
			if f.isID && f.id == "~" && s.dir != 'w' {
				t.Errorf("%s: only the pod worker can send an ID that is new or not", typ)
			}
		}
	}
}

func TestWasiHandleRegistry(t *testing.T) {
	testPeer(t, nil, nil)
	wasiHandle("bind", func(*wasiPeer, wasiMsg) {})
	for _, typ := range []string{"bind", "bound", "no-such-message"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("wasiHandle(%q) does not panic", typ)
				}
			}()
			wasiHandle(typ, func(*wasiPeer, wasiMsg) {})
		}()
	}
}

// MS1: each message from the pod worker is examined before its handler
// runs. A message that fails does not reach the handler.
func TestWasiDispatchExamines(t *testing.T) {
	var got []string
	h := func(_ *wasiPeer, m wasiMsg) { got = append(got, m.Type) }
	p, _ := testPeer(t, map[string]wasiHandler{"stdout": h, "exit": h, "mem": h, "bind": h, "data": h, "lookup": h, "cpu": h}, nil)

	good := []js.Value{
		msg(map[string]any{"type": "stdout", "bytes": buf(10)}),
		msg(map[string]any{"type": "exit", "code": 1}),
		msg(map[string]any{"type": "exit", "code": 137, "error": "killed"}),
		msg(map[string]any{"type": "mem", "size": 65536}),
		msg(map[string]any{"type": "cpu", "ms": 1.5}),
		msg(map[string]any{"type": "bind", "socket": 1, "protocol": "tcp", "family": "ipv6", "address": "::", "port": 0}),
		msg(map[string]any{"type": "data", "socket": 1, "bytes": buf(256 << 10)}),
		msg(map[string]any{"type": "lookup", "lookup": 7, "name": "kubernetes"}),
	}
	for _, m := range good {
		p.dispatch(m)
	}
	if want := "stdout exit exit mem cpu bind data lookup"; strings.Join(got, " ") != want {
		t.Fatalf("handled %q, want %q", strings.Join(got, " "), want)
	}
	if p.Drops() != 0 {
		t.Fatalf("%d drops of correct messages", p.Drops())
	}

	bad := map[string]js.Value{
		"not an object":         js.ValueOf("stdout"),
		"an array":              js.Global().Get("Array").New(),
		"no type":               msg(map[string]any{"bytes": buf(1)}),
		"unknown type":          msg(map[string]any{"type": "format-disk"}),
		"wrong direction":       msg(map[string]any{"type": "start", "args": []any{}, "env": []any{}, "cwd": "/"}),
		"missing field":         msg(map[string]any{"type": "exit"}),
		"unknown field":         msg(map[string]any{"type": "exit", "code": 0, "extra": 1}),
		"code not a number":     msg(map[string]any{"type": "exit", "code": "0"}),
		"code out of range":     msg(map[string]any{"type": "exit", "code": 256}),
		"code not an integer":   msg(map[string]any{"type": "exit", "code": 1.5}),
		"error text too long":   msg(map[string]any{"type": "exit", "code": 1, "error": strings.Repeat("x", 4097)}),
		"bytes not a buffer":    msg(map[string]any{"type": "stdout", "bytes": "text"}),
		"stdout above 1 MiB":    msg(map[string]any{"type": "stdout", "bytes": buf(1<<20 + 1)}),
		"negative size":         msg(map[string]any{"type": "mem", "size": -1}),
		"cpu not finite":        msg(map[string]any{"type": "cpu", "ms": js.Global().Get("Infinity")}),
		"bad protocol":          msg(map[string]any{"type": "bind", "socket": 2, "protocol": "sctp", "family": "ipv4", "address": "0.0.0.0", "port": 0}),
		"bad address":           msg(map[string]any{"type": "bind", "socket": 2, "protocol": "tcp", "family": "ipv4", "address": "10.0.0.256", "port": 0}),
		"address with zone":     msg(map[string]any{"type": "bind", "socket": 2, "protocol": "tcp", "family": "ipv6", "address": "fe80::1%eth0", "port": 0}),
		"bad port":              msg(map[string]any{"type": "bind", "socket": 2, "protocol": "tcp", "family": "ipv4", "address": "0.0.0.0", "port": 65536}),
		"data above the credit": msg(map[string]any{"type": "data", "socket": 1, "bytes": buf(256<<10 + 1)}),
		"socket ID 0":           msg(map[string]any{"type": "data", "socket": 0, "bytes": buf(1)}),
		"empty name":            msg(map[string]any{"type": "lookup", "lookup": 8, "name": ""}),
		"name too long":         msg(map[string]any{"type": "lookup", "lookup": 9, "name": strings.Repeat("a", 254)}),
	}
	for why, m := range bad {
		before, n := p.Drops(), len(got)
		p.dispatch(m)
		if len(got) != n {
			t.Errorf("%s: the handler got the message", why)
		}
		if p.Drops() != before+1 {
			t.Errorf("%s: the message was not dropped", why)
		}
	}
}

// MS2, MS6: IDs must belong to the container. A message that fails closes
// its socket or request: the Go side releases it, and the pod worker gets
// the message that ends it.
func TestWasiDispatchClosesOnFailure(t *testing.T) {
	var handled []string
	h := func(_ *wasiPeer, m wasiMsg) { handled = append(handled, m.Type) }
	var closed []string
	closer := func(kind string) func(*wasiPeer, uint32) {
		return func(_ *wasiPeer, id uint32) { closed = append(closed, kind+":"+strconv.Itoa(int(id))) }
	}
	p, posted := testPeer(t,
		map[string]wasiHandler{"bind": h, "data": h, "fetch": h, "response": h, "lookup": h, "closed": h},
		map[string][]func(*wasiPeer, uint32){"socket": {closer("socket")}, "fetch": {closer("fetch")}, "request": {closer("request")}})
	lastPost := func() string {
		if len(*posted) == 0 {
			return ""
		}
		v := (*posted)[len(*posted)-1]
		keys := js.Global().Get("Object").Call("keys", v).Call("sort")
		return js.Global().Get("JSON").Call("stringify", v, keys).String()
	}

	// A bind makes socket 5 a socket of the container.
	p.dispatch(msg(map[string]any{"type": "bind", "socket": 5, "protocol": "udp", "family": "ipv4", "address": "0.0.0.0", "port": 53}))
	if !p.Owns("socket", 5) {
		t.Fatal("bind did not make socket 5 a socket of the container")
	}
	// A second bind of the same socket is allowed (~).
	p.dispatch(msg(map[string]any{"type": "bind", "socket": 5, "protocol": "udp", "family": "ipv4", "address": "0.0.0.0", "port": 53}))
	if len(handled) != 2 {
		t.Fatalf("handled %v", handled)
	}

	// A foreign socket: dropped, and the pod worker gets "closed".
	p.dispatch(msg(map[string]any{"type": "data", "socket": 6, "bytes": buf(1)}))
	if len(handled) != 2 || lastPost() != `{"error":"invalid-argument","socket":6,"type":"closed"}` {
		t.Fatalf("foreign socket: handled %v, posted %s", handled, lastPost())
	}
	if len(closed) != 0 {
		t.Fatalf("a foreign socket ran the closers: %v", closed)
	}
	// An ID of the Go side cannot be a new socket of the pod worker.
	p.dispatch(msg(map[string]any{"type": "bind", "socket": wasiGoIDs, "protocol": "tcp", "family": "ipv4", "address": "0.0.0.0", "port": 80}))
	if p.Owns("socket", wasiGoIDs) || len(handled) != 2 {
		t.Fatal("the pod worker made an ID of the Go side")
	}

	// A size above the limit on socket 5: dropped, closed on the two sides.
	p.dispatch(msg(map[string]any{"type": "data", "socket": 5, "bytes": buf(256<<10 + 1)}))
	if p.Owns("socket", 5) || strings.Join(closed, ",") != "socket:5" {
		t.Fatalf("socket 5 is not closed: closers %v", closed)
	}
	if lastPost() != `{"error":"invalid-argument","socket":5,"type":"closed"}` {
		t.Fatalf("posted %s", lastPost())
	}
	// The socket is closed: the next data is foreign.
	p.dispatch(msg(map[string]any{"type": "data", "socket": 5, "bytes": buf(1)}))
	if len(handled) != 2 {
		t.Fatal("data on a closed socket reached the handler")
	}

	// A fetch with a URL that is too long: the fetch fails at once.
	p.dispatch(msg(map[string]any{"type": "fetch", "fetch": 3, "method": "GET", "url": strings.Repeat("u", 8193),
		"headers": js.Global().Get("Array").New(), "body": buf(0)}))
	if p.Owns("fetch", 3) || lastPost() != `{"error":"internal-error","fetch":3,"type":"fetched"}` {
		t.Fatalf("fetch: posted %s", lastPost())
	}
	// A header with a value that is not bytes.
	hdrs := js.Global().Get("Array").New(js.Global().Get("Array").New("cookie", "a=b"))
	p.dispatch(msg(map[string]any{"type": "fetch", "fetch": 4, "method": "GET", "url": "http://web/", "headers": hdrs, "body": buf(0)}))
	if p.Owns("fetch", 4) || lastPost() != `{"error":"internal-error","fetch":4,"type":"fetched"}` {
		t.Fatalf("fetch with bad headers: posted %s", lastPost())
	}
	// A body above 16 MiB on an open fetch of the container.
	p.dispatch(msg(map[string]any{"type": "fetch", "fetch": 10, "method": "POST", "url": "http://web/", "headers": js.Global().Get("Array").New(), "body": buf(1)}))
	if !p.Owns("fetch", 10) {
		t.Fatal("fetch 10 is not open")
	}
	// The same ID again is not a new ID.
	p.dispatch(msg(map[string]any{"type": "fetch", "fetch": 10, "method": "GET", "url": "http://web/", "headers": js.Global().Get("Array").New(), "body": buf(1)}))
	if p.Owns("fetch", 10) || !strings.Contains(strings.Join(closed, ","), "fetch:10") {
		t.Fatalf("a second fetch 10 did not close fetch 10: closers %v", closed)
	}

	// A lookup with a name that is too long.
	p.dispatch(msg(map[string]any{"type": "lookup", "lookup": 2, "name": strings.Repeat("a", 300)}))
	if lastPost() != `{"error":"invalid-argument","lookup":2,"type":"addresses"}` {
		t.Fatalf("lookup: posted %s", lastPost())
	}

	// A response for a request that the Go side did not send.
	p.dispatch(msg(map[string]any{"type": "response", "request": 9, "status": 200}))
	if lastPost() != `{"request":9,"type":"cancel"}` {
		t.Fatalf("response: posted %s", lastPost())
	}
	// A request of the Go side, then a response with a bad status.
	id := p.NewID("request")
	if err := p.send("request", map[string]any{"request": id, "method": "GET", "url": "/", "headers": js.Global().Get("Array").New(), "body": []byte{}}); err != nil {
		t.Fatal(err)
	}
	p.dispatch(msg(map[string]any{"type": "response", "request": id, "status": 99}))
	if p.Owns("request", id) || !strings.Contains(strings.Join(closed, ","), "request:") {
		t.Fatalf("bad response: request is open, closers %v", closed)
	}
	if !strings.Contains(lastPost(), `"type":"cancel"`) {
		t.Fatalf("bad response: posted %s", lastPost())
	}

	if want := "bind bind fetch"; strings.Join(handled, " ") != want {
		t.Fatalf("handled %q, want %q", strings.Join(handled, " "), want)
	}
}

// A message that ends an ID releases it after the handler.
func TestWasiDispatchEndsIDs(t *testing.T) {
	p, _ := testPeer(t, map[string]wasiHandler{"connect": func(*wasiPeer, wasiMsg) {}, "closed": func(*wasiPeer, wasiMsg) {}}, nil)
	p.dispatch(msg(map[string]any{"type": "connect", "socket": 1, "protocol": "tcp", "address": "10.96.0.1", "port": 443}))
	if !p.Owns("socket", 1) {
		t.Fatal("connect did not open socket 1")
	}
	p.dispatch(msg(map[string]any{"type": "closed", "socket": 1}))
	if p.Owns("socket", 1) {
		t.Fatal("closed did not end socket 1")
	}
}

func TestWasiSend(t *testing.T) {
	var closed []uint32
	p, posted := testPeer(t, nil, map[string][]func(*wasiPeer, uint32){"socket": {func(_ *wasiPeer, id uint32) { closed = append(closed, id) }}})
	// A message that the Go side does not send.
	if err := p.send("bind", map[string]any{"socket": 1, "protocol": "tcp", "family": "ipv4", "address": "0.0.0.0", "port": 1}); err == nil {
		t.Error("Go sent bind")
	}
	// A socket that does not belong to the container.
	if err := p.send("bound", map[string]any{"socket": 1, "address": "10.244.0.5", "port": 80}); err == nil {
		t.Error("Go sent bound for a foreign socket")
	}
	// accepted makes a new socket of the Go side.
	p.dispatch(msg(map[string]any{"type": "bind", "socket": 1, "protocol": "tcp", "family": "ipv4", "address": "0.0.0.0", "port": 80}))
	nid := p.NewID("socket")
	if nid < wasiGoIDs {
		t.Fatalf("NewID gives %d", nid)
	}
	err := p.send("accepted", map[string]any{"socket": 1, "accepted": nid, "localAddress": "10.244.0.5", "localPort": 80, "remoteAddress": "10.244.0.6", "remotePort": 40000})
	if err != nil || !p.Owns("socket", nid) {
		t.Fatalf("accepted: %v, owns %v", err, p.Owns("socket", nid))
	}
	// Bytes go as a transferred ArrayBuffer.
	if err := p.send("data", map[string]any{"socket": nid, "bytes": []byte("hi")}); err != nil {
		t.Fatal(err)
	}
	last := (*posted)[len(*posted)-1]
	if got := string(jsBytes(js.Global().Get("Uint8Array").New(last.Get("bytes")))); got != "hi" {
		t.Fatalf("data: %q", got)
	}
	// Data above the limit is an error of the Go code.
	if err := p.send("data", map[string]any{"socket": nid, "bytes": make([]byte, 256<<10+1)}); err == nil {
		t.Error("Go sent data above the limit")
	}
	// closed ends the socket.
	if err := p.send("closed", map[string]any{"socket": nid}); err != nil || p.Owns("socket", nid) {
		t.Fatalf("closed: %v", err)
	}
	// Close releases the IDs that remain and runs the closers.
	p.Close()
	if p.Owns("socket", 1) || len(closed) != 1 || closed[0] != 1 {
		t.Fatalf("Close: closers %v", closed)
	}
	if err := p.send("stdin", map[string]any{"bytes": []byte{}, "end": true}); err == nil {
		t.Error("send after Close")
	}
}

// The CLI messages of a component go to the container (CO5): stdout,
// stderr and the exit code.
func TestWasiCLIMessages(t *testing.T) {
	p, _ := testPeer(t, nil, nil)
	// The handlers of tabnode_wasi.go.
	for typ, h := range wasiCLIHandlers {
		wasiHandlers[typ] = h
	}
	var out, errOut bytes.Buffer
	var code = -1
	var exitMsg string
	var mem int64
	p.cli = &wasiCLI{stdout: &out, stderr: &errOut, exit: func(c int, m string) { code, exitMsg = c, m }, mem: func(n int64) { mem = n }}
	bytesOf := func(s string) js.Value {
		a := js.Global().Get("Uint8Array").New(len(s))
		js.CopyBytesToJS(a, []byte(s))
		return a.Get("buffer")
	}
	p.dispatch(msg(map[string]any{"type": "stdout", "bytes": bytesOf("arg first\n")}))
	p.dispatch(msg(map[string]any{"type": "stderr", "bytes": bytesOf("exit 1\n")}))
	p.dispatch(msg(map[string]any{"type": "mem", "size": 1 << 20}))
	p.dispatch(msg(map[string]any{"type": "exit", "code": 1}))
	if out.String() != "arg first\n" || errOut.String() != "exit 1\n" || code != 1 || exitMsg != "" || mem != 1<<20 {
		t.Fatalf("stdout %q, stderr %q, code %d %q, mem %d", out.String(), errOut.String(), code, exitMsg, mem)
	}
}

// The world and the imports of a component (the part of W1.4a: other
// components get a clear "unsupported" text).
func TestComponentUnsupported(t *testing.T) {
	cmdImports := []string{"wasi:cli/environment", "wasi:io/streams", "wasi:cli/environment@0.2.3", "wasi:cli/exit@0.2.3", "wasi:io/streams@0.2.3", "wasi:cli/stdout@0.2.3",
		"wasi:filesystem/preopens@0.2.3", "wasi:sockets/tcp@0.2.3", "wasi:clocks/wall-clock@0.2.9", "wasi:random/insecure-seed@0.2.12"}
	for _, c := range []struct {
		imports, exports []string
		want             string
	}{
		{cmdImports, []string{"wasi:cli/run@0.2.3"}, ""},
		{cmdImports, []string{"wasi:cli/run@0.2.3", "wasi:http/incoming-handler@0.2.3"}, ""},
		{cmdImports, []string{"wasi:http/incoming-handler@0.2.3", "wasi:cli/run@0.2.3"}, "proxy component"},
		{cmdImports, []string{"go"}, "without the export wasi:cli/run"},
		{nil, nil, "without the export wasi:cli/run"},
		{append(cmdImports, "wasi:http/types@0.2.12"), []string{"wasi:cli/run@0.2.3"}, "imports wasi:http/types@0.2.12"},
		{append(cmdImports, "wasi:http/outgoing-handler"), []string{"run", "wasi:cli/run@0.2.3"}, "imports wasi:http/outgoing-handler"},
		{append(cmdImports, "wasi:cli/environment@0.3.0"), []string{"wasi:cli/run@0.2.3"}, "imports wasi:cli/environment@0.3.0"},
		{append(cmdImports, "wasi:keyvalue/store@0.2.0"), []string{"wasi:cli/run@0.2.3"}, "imports wasi:keyvalue/store@0.2.0"},
		{append(cmdImports, "log"), []string{"wasi:cli/run@0.2.3"}, "imports log,"},
	} {
		got := componentUnsupported(c.imports, c.exports)
		if (c.want == "") != (got == "") || !strings.Contains(got, c.want) {
			t.Errorf("imports %v, exports %v: %q, want %q", c.imports, c.exports, got, c.want)
		}
	}
}
