package main

// The messages between the pod worker of a component and the Go side
// (docs/wasi-sockets.md, section 8.3). web/src/wasi_messages.js has the same
// table: a new message or field goes into the two files in the same pull
// request (web/test/wasi_messages.test.mjs compares them).
//
// Each pod worker of a component has a wasiPeer, the Go end of its message
// channel. The peer examines each message from the pod worker before its
// handler runs (MS1): a known type in the correct direction, the type and the
// size of each field (section 8.4), and the IDs that belong to the container
// (MS2). It drops a message that fails, and closes the socket or the request
// of the message (MS6).
//
// Handlers are in a registry by message type. A later package registers its
// handlers in an init function of its own file, for example
//
//	func init() {
//		wasiHandle("bind", func(p *wasiPeer, m wasiMsg) { ... })
//		wasiOnClose("socket", func(p *wasiPeer, id uint32) { ... })
//	}
//
// and sends with p.send("bound", map[string]any{"socket": id, ...}).

import (
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net/netip"
	"regexp"
	"sort"
	"strings"
	"sync"
	"syscall/js"
)

// wasiMessages gives the direction and the fields of each message, in the
// format of wasi_messages.js:
//
//	"<direction> <field>:<kind> <field>?:<kind> ..."
//
// direction: w (pod worker to Go), g (Go to pod worker), b (the two). A field
// with "?" is optional. An ID kind (socket, request, fetch, lookup) has a
// suffix: none (the ID must belong to the container), "+" (a new ID), "~"
// (an ID of the container or a new ID), "-" (the message ends the ID).
var wasiMessages = map[string]string{
	"ready":        "w",
	"start":        "g component:component args:strings env:strings cwd:path preopens?:preopens",
	"exit":         "w code:code error?:text",
	"stdout":       "w bytes:chunk",
	"stderr":       "w bytes:chunk",
	"stdin":        "g bytes:chunk end:bool",
	"mem":          "w size:size",
	"cpu":          "w ms:ms",
	"bind":         "w socket:socket~ protocol:protocol family:family address:ip port:port",
	"bound":        "g socket:socket address?:ip port?:port error?:errcode",
	"listen":       "w socket:socket backlog:backlog",
	"listening":    "g socket:socket error?:errcode",
	"accepted":     "g socket:socket accepted:socket+ localAddress:ip localPort:port remoteAddress:ip remotePort:port",
	"connect":      "w socket:socket~ protocol:protocol address:ip port:port",
	"connected":    "g socket:socket localAddress?:ip localPort?:port remoteAddress?:ip remotePort?:port error?:errcode",
	"data":         "b socket:socket bytes:data",
	"datagram":     "b socket:socket address:ip port:port bytes:datagram",
	"credit":       "b socket:socket kind:credit n:credits",
	"error":        "g socket:socket error:errcode",
	"shutdown":     "w socket:socket how:how",
	"closed":       "b socket:socket- error?:errcode",
	"lookup":       "w lookup:lookup+ name:hostname",
	"addresses":    "g lookup:lookup- addresses?:ips error?:errcode",
	"request":      "g request:request+ method:method url:url headers:headers body:body",
	"response":     "w request:request- status?:status headers?:headers body?:body error?:errcode",
	"cancel":       "g request:request",
	"fetch":        "w fetch:fetch+ method:method url:url headers:headers body:body",
	"fetched":      "g fetch:fetch- status?:status headers?:headers body?:body error?:errcode",
	"fetch-cancel": "w fetch:fetch-",
}

// wasiLimits are the limits of the fields (section 8.4), the same numbers as
// LIMITS in wasi_messages.js.
var wasiLimits = map[string]int{
	"chunk":       1 << 20,
	"data":        256 << 10,
	"datagram":    65535,
	"body":        16 << 20,
	"credits":     256 << 10,
	"backlog":     4096,
	"text":        4096,
	"path":        4096,
	"hostname":    253,
	"url":         8192,
	"method":      64,
	"headers":     1000,
	"headerName":  256,
	"headerValue": 64 << 10,
	"ips":         256,
	"strings":     65536,
	"string":      1 << 20,
	"preopens":    256,
}

// The IDs of the pod worker are 1 to wasiGoIDs-1, those of the Go side
// wasiGoIDs to 0xffffffff.
const wasiGoIDs = 0x80000000

// wasiCloseMsg gives the message that ends an ID of each kind on the side of
// the pod worker, for MS6.
var wasiCloseMsg = map[string]func(id uint32) map[string]any{
	"socket": func(id uint32) map[string]any {
		return map[string]any{"type": "closed", "socket": id, "error": "invalid-argument"}
	},
	"request": func(id uint32) map[string]any { return map[string]any{"type": "cancel", "request": id} },
	"fetch": func(id uint32) map[string]any {
		return map[string]any{"type": "fetched", "fetch": id, "error": "internal-error"}
	},
	"lookup": func(id uint32) map[string]any {
		return map[string]any{"type": "addresses", "lookup": id, "error": "invalid-argument"}
	},
}

type wasiField struct {
	name     string
	kind     string
	id       string // "", "+", "~" or "-" for an ID kind
	isID     bool
	optional bool
}

type wasiSpec struct {
	dir    byte // 'w', 'g' or 'b'
	fields []wasiField
}

var wasiFieldRE = regexp.MustCompile(`^([a-zA-Z]+)(\??):([a-z]+)([+~-]?)$`)

var wasiSpecs = parseWasiMessages(wasiMessages)

func parseWasiMessages(m map[string]string) map[string]*wasiSpec {
	specs := map[string]*wasiSpec{}
	for typ, s := range m {
		parts := strings.Fields(s)
		if len(parts) == 0 || len(parts[0]) != 1 || !strings.Contains("wgb", parts[0]) {
			panic("wasi message " + typ + ": no direction")
		}
		spec := &wasiSpec{dir: parts[0][0]}
		for _, f := range parts[1:] {
			sm := wasiFieldRE.FindStringSubmatch(f)
			if sm == nil || wasiKinds[sm[3]] == nil {
				panic("wasi message " + typ + ": field " + f)
			}
			isID := wasiIDKinds[sm[3]]
			if sm[4] != "" && !isID {
				panic("wasi message " + typ + ": field " + f + ": only an ID has a suffix")
			}
			spec.fields = append(spec.fields, wasiField{name: sm[1], kind: sm[3], id: sm[4], isID: isID, optional: sm[2] == "?"})
		}
		specs[typ] = spec
	}
	return specs
}

var wasiIDKinds = map[string]bool{"socket": true, "request": true, "fetch": true, "lookup": true}

// js values that the examination uses.
var (
	jsArrayBufferCtor = js.Global().Get("ArrayBuffer")
	jsArrayCtor       = js.Global().Get("Array")
	jsObjectCtor      = js.Global().Get("Object")
	jsStringCtor      = js.Global().Get("String")
)

func jsInt(v js.Value, min, max float64) bool {
	if v.Type() != js.TypeNumber {
		return false
	}
	f := v.Float()
	return f == math.Trunc(f) && f >= min && f <= max
}

// jsStrLen gives the length of a JavaScript string without a copy into Go
// memory (Value.Length works only on objects).
func jsStrLen(v js.Value) int { return jsStringCtor.New(v).Length() }

func jsStr(v js.Value, max int, test func(string) bool) bool {
	if v.Type() != js.TypeString || jsStrLen(v) > max {
		return false
	}
	return test == nil || test(v.String())
}

func jsBuf(v js.Value, max int) bool {
	return v.Type() == js.TypeObject && v.InstanceOf(jsArrayBufferCtor) && v.Get("byteLength").Int() <= max
}

func jsList(v js.Value, max int, each func(js.Value) bool) bool {
	if v.Type() != js.TypeObject || !jsArrayCtor.Call("isArray", v).Bool() || v.Length() > max {
		return false
	}
	for i := 0; i < v.Length(); i++ {
		if !each(v.Index(i)) {
			return false
		}
	}
	return true
}

func oneOf(a ...string) func(js.Value) bool {
	return func(v js.Value) bool {
		if v.Type() != js.TypeString {
			return false
		}
		s := v.String()
		for _, x := range a {
			if s == x {
				return true
			}
		}
		return false
	}
}

var (
	errcodeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]*$`)
	methodRE  = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
)

// wasiIP accepts the text forms of IPv4 and IPv6 addresses, without a zone.
func wasiIP(s string) bool {
	a, err := netip.ParseAddr(s)
	return err == nil && a.Zone() == ""
}

func jsIP(v js.Value) bool { return jsStr(v, 45, wasiIP) }

// wasiKinds gives the test of each kind of field. It is a variable with an
// initializer, not an init function: the init functions of other files
// register handlers with the parsed table.
var wasiKinds = makeWasiKinds()

func makeWasiKinds() map[string]func(js.Value) bool {
	l := wasiLimits
	return map[string]func(js.Value) bool{
		"socket":   func(v js.Value) bool { return jsInt(v, 1, math.MaxUint32) },
		"request":  func(v js.Value) bool { return jsInt(v, 1, math.MaxUint32) },
		"fetch":    func(v js.Value) bool { return jsInt(v, 1, math.MaxUint32) },
		"lookup":   func(v js.Value) bool { return jsInt(v, 1, math.MaxUint32) },
		"bool":     func(v js.Value) bool { return v.Type() == js.TypeBoolean },
		"code":     func(v js.Value) bool { return jsInt(v, 0, 255) },
		"size":     func(v js.Value) bool { return jsInt(v, 0, 1<<53-1) },
		"ms":       func(v js.Value) bool { return v.Type() == js.TypeNumber && v.Float() >= 0 && v.Float() <= 3600e3 },
		"port":     func(v js.Value) bool { return jsInt(v, 0, 65535) },
		"backlog":  func(v js.Value) bool { return jsInt(v, 1, float64(l["backlog"])) },
		"credits":  func(v js.Value) bool { return jsInt(v, 0, float64(l["credits"])) },
		"status":   func(v js.Value) bool { return jsInt(v, 100, 999) },
		"protocol": oneOf("tcp", "udp"),
		"family":   oneOf("ipv4", "ipv6"),
		"how":      oneOf("receive", "send", "both"),
		"credit":   oneOf("bytes", "datagram-bytes", "connections"),
		"errcode":  func(v js.Value) bool { return jsStr(v, 64, errcodeRE.MatchString) },
		"text":     func(v js.Value) bool { return jsStr(v, l["text"], nil) },
		"path": func(v js.Value) bool {
			return jsStr(v, l["path"], func(s string) bool { return strings.HasPrefix(s, "/") })
		},
		"hostname": func(v js.Value) bool { return jsStr(v, l["hostname"], func(s string) bool { return s != "" }) },
		"url":      func(v js.Value) bool { return jsStr(v, l["url"], func(s string) bool { return s != "" }) },
		"method":   func(v js.Value) bool { return jsStr(v, l["method"], methodRE.MatchString) },
		"ip":       jsIP,
		"ips":      func(v js.Value) bool { return jsList(v, l["ips"], jsIP) },
		"strings": func(v js.Value) bool {
			return jsList(v, l["strings"], func(s js.Value) bool { return jsStr(s, l["string"], nil) })
		},
		"headers": func(v js.Value) bool {
			return jsList(v, l["headers"], func(h js.Value) bool {
				return jsList(h, 2, func(js.Value) bool { return true }) && h.Length() == 2 &&
					jsStr(h.Index(0), l["headerName"], func(s string) bool { return s != "" }) && jsBuf(h.Index(1), l["headerValue"])
			})
		},
		"chunk":    func(v js.Value) bool { return jsBuf(v, l["chunk"]) },
		"data":     func(v js.Value) bool { return jsBuf(v, l["data"]) },
		"datagram": func(v js.Value) bool { return jsBuf(v, l["datagram"]) },
		"body":     func(v js.Value) bool { return jsBuf(v, l["body"]) },
		// The copy of wasmPreopens (section 8.10). Its size limit is
		// wasmSnapshotLimit.
		"preopens": func(v js.Value) bool {
			return jsList(v, l["preopens"], func(p js.Value) bool {
				return p.Type() == js.TypeObject && jsStr(p.Get("guest"), l["path"], func(s string) bool { return strings.HasPrefix(s, "/") }) &&
					p.Get("readOnly").Type() == js.TypeBoolean &&
					jsArrayCtor.Call("isArray", p.Get("files")).Bool() && jsArrayCtor.Call("isArray", p.Get("dirs")).Bool()
			})
		},
		"component": func(v js.Value) bool {
			return v.Type() == js.TypeObject && v.Get("js").Type() == js.TypeString && v.Get("modules").Type() == js.TypeObject
		},
	}
}

// wasiMsg is a message that passed the examination.
type wasiMsg struct {
	Type string
	V    js.Value
}

func (m wasiMsg) Has(f string) bool       { return m.V.Get(f).Type() != js.TypeUndefined }
func (m wasiMsg) Str(f string) string     { return m.V.Get(f).String() }
func (m wasiMsg) Int(f string) int        { return m.V.Get(f).Int() }
func (m wasiMsg) Uint(f string) uint32    { return uint32(m.V.Get(f).Float()) }
func (m wasiMsg) Float(f string) float64  { return m.V.Get(f).Float() }
func (m wasiMsg) Bool(f string) bool      { return m.V.Get(f).Bool() }
func (m wasiMsg) Value(f string) js.Value { return m.V.Get(f) }

// Bytes copies an ArrayBuffer field into Go memory.
func (m wasiMsg) Bytes(f string) []byte {
	v := m.V.Get(f)
	if v.Type() != js.TypeObject {
		return nil
	}
	return jsBytes(js.Global().Get("Uint8Array").New(v))
}

// wasiHandler handles a message from a pod worker. It runs on the event loop
// of the Go thread: it must not wait.
type wasiHandler func(p *wasiPeer, m wasiMsg)

var (
	wasiHandlers = map[string]wasiHandler{}
	wasiClosers  = map[string][]func(p *wasiPeer, id uint32){}
)

// wasiHandle registers the handler of a message type from the pod worker.
// Call it from an init function.
func wasiHandle(typ string, h wasiHandler) {
	spec := wasiSpecs[typ]
	if spec == nil || spec.dir == 'g' {
		panic("wasiHandle: the pod worker does not send " + typ)
	}
	if wasiHandlers[typ] != nil {
		panic("wasiHandle: " + typ + " has a handler already")
	}
	wasiHandlers[typ] = h
}

// wasiOnClose registers a function that releases the Go side of an ID of a
// kind (socket, request, fetch, lookup) when a message fails the
// examination (MS6), or when the container stops. Call it from an init
// function.
func wasiOnClose(kind string, fn func(p *wasiPeer, id uint32)) {
	if !wasiIDKinds[kind] {
		panic("wasiOnClose: unknown kind " + kind)
	}
	wasiClosers[kind] = append(wasiClosers[kind], fn)
}

// wasiCLI is the CLI of the container of a pod worker.
type wasiCLI struct {
	stdout, stderr io.Writer
	exit           func(code int, msg string)
	mem            func(bytes int64)
}

// wasiPeer is the Go end of the message channel of one pod worker. It is
// for one container.
type wasiPeer struct {
	name string
	post func(msg, transfer js.Value)
	cli  *wasiCLI

	mu     sync.Mutex
	ids    map[string]map[uint32]bool
	next   uint32
	drops  int
	state  map[string]any
	closed bool
}

func newWasiPeer(name string, post func(msg, transfer js.Value)) *wasiPeer {
	return &wasiPeer{name: name, post: post, ids: map[string]map[uint32]bool{}, next: wasiGoIDs, state: map[string]any{}}
}

// State gives the value of a later package for this peer under key, made
// with mk the first time.
func (p *wasiPeer) State(key string, mk func() any) any {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.state[key]
	if !ok {
		v = mk()
		p.state[key] = v
	}
	return v
}

// Owns reports whether the ID of a kind belongs to the container.
func (p *wasiPeer) Owns(kind string, id uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ids[kind][id]
}

// NewID makes a new ID of the Go side for a kind. It belongs to the
// container when a message with it goes out.
func (p *wasiPeer) NewID(kind string) uint32 {
	p.mu.Lock()
	defer p.mu.Unlock()
	for {
		id := p.next
		p.next++
		if p.next == 0 {
			p.next = wasiGoIDs
		}
		if !p.ids[kind][id] {
			return id
		}
	}
}

func (p *wasiPeer) claim(kind string, id uint32) {
	if p.ids[kind] == nil {
		p.ids[kind] = map[uint32]bool{}
	}
	p.ids[kind][id] = true
}

// Release ends an ID of the container.
func (p *wasiPeer) Release(kind string, id uint32) {
	p.mu.Lock()
	delete(p.ids[kind], id)
	p.mu.Unlock()
}

// Close releases all IDs of the container: the container stopped. The
// functions of wasiOnClose run for each ID.
func (p *wasiPeer) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	ids := p.ids
	p.ids = map[string]map[uint32]bool{}
	p.mu.Unlock()
	kinds := make([]string, 0, len(ids))
	for k := range ids {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range kinds {
		for id := range ids[k] {
			for _, fn := range wasiClosers[k] {
				fn(p, id)
			}
		}
	}
}

// idCheck examines an ID field against the IDs of the container. fromGo is
// true for a message that the Go side sends.
func (p *wasiPeer) idCheck(f wasiField, id uint32, fromGo bool) error {
	owned := p.ids[f.kind][id]
	switch f.id {
	case "", "-":
		if !owned {
			return fmt.Errorf("%s %d does not belong to the container", f.kind, id)
		}
	case "+", "~":
		if f.id == "~" && owned {
			return nil
		}
		if owned {
			return fmt.Errorf("%s %d is not a new ID", f.kind, id)
		}
		if !fromGo && id >= wasiGoIDs {
			return fmt.Errorf("%s %d is not an ID that the pod worker can make", f.kind, id)
		}
	}
	return nil
}

// wasiTarget is the ID that a message is about: the first ID field.
type wasiTarget struct {
	kind string
	id   uint32
	ok   bool
}

// examine examines a message in the direction dir ('w' from the pod
// worker, 'g' from the Go side). It gives the type and the first ID of the
// message also when the message fails, so that MS6 can close it. The
// caller holds p.mu.
func (p *wasiPeer) examine(v js.Value, dir byte) (string, wasiTarget, error) {
	var t wasiTarget
	if v.Type() != js.TypeObject || jsArrayCtor.Call("isArray", v).Bool() {
		return "", t, errors.New("the message is not an object")
	}
	tv := v.Get("type")
	if tv.Type() != js.TypeString || jsStrLen(tv) > 64 {
		return "", t, errors.New("the message has no type")
	}
	typ := tv.String()
	spec := wasiSpecs[typ]
	if spec == nil {
		return typ, t, fmt.Errorf("unknown message type %q", typ)
	}
	// The ID first: a message that fails later still closes it.
	for _, f := range spec.fields {
		if f.isID {
			if fv := v.Get(f.name); wasiKinds[f.kind](fv) {
				t = wasiTarget{kind: f.kind, id: uint32(fv.Float()), ok: true}
			}
			break
		}
	}
	if spec.dir != 'b' && spec.dir != dir {
		return typ, t, fmt.Errorf("%s does not go in this direction", typ)
	}
	keys := jsObjectCtor.Call("keys", v)
	if keys.Length() > len(spec.fields)+1 {
		return typ, t, fmt.Errorf("%s: too many fields", typ)
	}
	for i := 0; i < keys.Length(); i++ {
		k := keys.Index(i).String()
		if k == "type" {
			continue
		}
		known := false
		for _, f := range spec.fields {
			if f.name == k {
				known = true
				break
			}
		}
		if !known {
			if len(k) > 64 {
				k = k[:64]
			}
			return typ, t, fmt.Errorf("%s: unknown field %q", typ, k)
		}
	}
	for _, f := range spec.fields {
		fv := v.Get(f.name)
		if fv.Type() == js.TypeUndefined {
			if f.optional {
				continue
			}
			return typ, t, fmt.Errorf("%s: field %s is missing", typ, f.name)
		}
		if !wasiKinds[f.kind](fv) {
			return typ, t, fmt.Errorf("%s: field %s is not a correct %s", typ, f.name, f.kind)
		}
		if f.isID {
			if err := p.idCheck(f, uint32(fv.Float()), dir == 'g'); err != nil {
				return typ, t, fmt.Errorf("%s: %v", typ, err)
			}
		}
	}
	return typ, t, nil
}

// apply claims the new IDs of a message that passed the examination, and
// gives the IDs that end after the handler. The caller holds p.mu.
func (p *wasiPeer) apply(typ string, v js.Value) (end []wasiTarget) {
	for _, f := range wasiSpecs[typ].fields {
		if !f.isID {
			continue
		}
		fv := v.Get(f.name)
		if fv.Type() == js.TypeUndefined {
			continue
		}
		id := uint32(fv.Float())
		switch f.id {
		case "+", "~":
			p.claim(f.kind, id)
		case "-":
			end = append(end, wasiTarget{kind: f.kind, id: id, ok: true})
		}
	}
	return end
}

// dispatch examines a message from the pod worker and gives it to its
// handler (MS1). A message that fails is dropped, and its socket or request
// is closed (MS6).
func (p *wasiPeer) dispatch(v js.Value) {
	p.mu.Lock()
	if p.closed {
		// After the stop, only the end of the pod worker matters.
		typ, _, err := p.examine(v, 'w')
		p.mu.Unlock()
		if h := wasiHandlers["exit"]; err == nil && typ == "exit" && h != nil {
			h(p, wasiMsg{Type: typ, V: v})
		}
		return
	}
	typ, t, err := p.examine(v, 'w')
	if err != nil {
		p.mu.Unlock()
		p.drop(t, err)
		return
	}
	end := p.apply(typ, v)
	p.mu.Unlock()
	if h := wasiHandlers[typ]; h != nil {
		h(p, wasiMsg{Type: typ, V: v})
	}
	for _, e := range end {
		p.Release(e.kind, e.id)
	}
}

// Fail does the steps of MS6 for a message that a handler finds wrong: the
// examination of the dispatcher passed, but the message does not agree with
// the state of its ID. An example is a "data" with more bytes than the credit
// (MS4). It counts the drop, writes a log line, releases the ID with the
// functions of wasiOnClose and sends the message that ends the ID
// (wasiCloseMsg). Call it from a handler, on the event loop.
func (p *wasiPeer) Fail(kind string, id uint32, reason string) {
	p.drop(wasiTarget{kind: kind, id: id, ok: wasiIDKinds[kind]}, errors.New(reason))
}

// drop counts a dropped message and closes its ID (MS6). The caller does not
// hold p.mu.
func (p *wasiPeer) drop(t wasiTarget, err error) {
	p.mu.Lock()
	owned := t.ok && p.ids[t.kind][t.id]
	if owned {
		delete(p.ids[t.kind], t.id)
	}
	p.drops++
	drops := p.drops
	closed := p.closed
	p.mu.Unlock()
	if drops <= 10 {
		log.Printf("tabnode: pod worker %s: dropped a message: %v", p.name, err)
	}
	if !t.ok || closed {
		return
	}
	if owned {
		for _, fn := range wasiClosers[t.kind] {
			fn(p, t.id)
		}
	}
	if mk := wasiCloseMsg[t.kind]; mk != nil {
		p.post(js.ValueOf(mk(t.id)), js.Undefined())
	}
}

// Drops gives the number of dropped messages.
func (p *wasiPeer) Drops() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.drops
}

// send sends a message to the pod worker. The values of fields are string,
// bool, the Go number types, []byte (an ArrayBuffer that is transferred),
// []string or js.Value. The message must obey the table: an error is an
// error of the Go code.
func (p *wasiPeer) send(typ string, fields map[string]any) error {
	obj := jsObjectCtor.New()
	obj.Set("type", typ)
	transfer := jsArrayCtor.New()
	for k, x := range fields {
		switch x := x.(type) {
		case []byte:
			arr := js.Global().Get("Uint8Array").New(len(x))
			js.CopyBytesToJS(arr, x)
			buf := arr.Get("buffer")
			obj.Set(k, buf)
			transfer.Call("push", buf)
		case []string:
			obj.Set(k, jsStrings(x))
		default:
			obj.Set(k, x)
		}
	}
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return errors.New("the pod worker is closed")
	}
	if _, _, err := p.examine(obj, 'g'); err != nil {
		p.mu.Unlock()
		return err
	}
	end := p.apply(typ, obj)
	for _, e := range end {
		delete(p.ids[e.kind], e.id)
	}
	p.mu.Unlock()
	p.post(obj, transfer)
	return nil
}
