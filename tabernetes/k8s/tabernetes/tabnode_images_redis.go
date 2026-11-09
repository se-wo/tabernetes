package main

// redis of the built-in images (tabnode_images.go): a small RESP server with
// the commands that tutorials use (strings, keys with expiry, lists, hashes,
// sets) and redis-cli. The data lives as long as the container, like a
// redis without persistence.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mvdan.cc/sh/v3/interp"
)

type redisVal struct {
	str    string
	list   []string
	hash   map[string]string
	set    map[string]bool
	kind   string // "string", "list", "hash", "set"
	expire time.Time
}

type redisDB struct {
	mu   sync.Mutex
	keys map[string]*redisVal
	pass string
}

// get returns a live key, dropping it once expired.
func (db *redisDB) get(k string) *redisVal {
	v := db.keys[k]
	if v != nil && !v.expire.IsZero() && time.Now().After(v.expire) {
		delete(db.keys, k)
		return nil
	}
	return v
}

// resp values: string (simple), []byte (bulk), nil (null bulk), int,
// []any (array), redisErr.
type redisErr string

const wrongType = redisErr("WRONGTYPE Operation against a key holding the wrong kind of value")

func redisServerProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	version := getenv(hc, "REDIS_VERSION")
	if version == "" {
		version = "8.2.2"
	}
	port := "6379"
	db := &redisDB{keys: map[string]*redisVal{}}
	confWarn := true
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-v" || a == "--version":
			fmt.Fprintf(hc.Stdout, "Redis server v=%s sha=00000000:0 malloc=jemalloc-5.3.0 bits=64 build=tabernetes\n", version)
			return nil
		case a == "--port" && i+1 < len(args):
			i++
			port = args[i]
		case a == "--requirepass" && i+1 < len(args):
			i++
			db.pass = args[i]
		case strings.HasPrefix(a, "--") && i+1 < len(args) && !strings.HasPrefix(args[i+1], "--"):
			i++ // --appendonly yes, --save "" and friends
		case !strings.HasPrefix(a, "-"):
			confWarn = false // a config file; its settings are not read
		}
	}
	now := func(role string) string {
		return "1:" + role + " " + time.Now().UTC().Format("02 Jan 2006 15:04:05.000")
	}
	out := &syncWriter{w: hc.Stdout}
	fmt.Fprintf(out, "%s * oO0OoO0OoO0Oo Redis is starting oO0OoO0OoO0Oo\n", now("C"))
	fmt.Fprintf(out, "%s * Redis version=%s, bits=64, commit=00000000, modified=0, pid=1, just started\n", now("C"), version)
	if confWarn {
		fmt.Fprintf(out, "%s # Warning: no config file specified, using the default config. In order to specify a config file use redis-server /path/to/redis.conf\n", now("C"))
	}
	ln, err := listenPod(ctx, ":"+port)
	if err != nil {
		fmt.Fprintf(out, "%s # Warning: Could not create server TCP listening socket *:%s: bind: Address already in use\n", now("M"), port)
		fmt.Fprintf(out, "%s # Failed listening on port %s (tcp), aborting.\n", now("M"), port)
		return interp.ExitStatus(1)
	}
	fmt.Fprintf(out, "%s * monotonic clock: POSIX clock_gettime\n", now("M"))
	fmt.Fprintf(out, "%s * Running mode=standalone, port=%s.\n", now("M"), port)
	fmt.Fprintf(out, "%s * Server initialized\n", now("M"))
	fmt.Fprintf(out, "%s * Ready to accept connections tcp\n", now("M"))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go redisConn(ctx, c, db, version, port, cancel, out)
		}
	}()
	<-ctx.Done()
	fmt.Fprintf(out, "%s # User requested shutdown...\n", now("M"))
	fmt.Fprintf(out, "%s # Redis is now ready to exit, bye bye...\n", now("M"))
	return nil
}

func redisConn(ctx context.Context, c net.Conn, db *redisDB, version, port string, shutdown func(), out io.Writer) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	r := bufio.NewReader(c)
	w := bufio.NewWriter(c)
	authed := db.pass == ""
	for {
		cmd, err := redisRead(r)
		if err != nil {
			return
		}
		args, ok := cmd.([]any)
		if !ok || len(args) == 0 {
			continue
		}
		argv := make([]string, len(args))
		for i, a := range args {
			argv[i] = fmt.Sprint(redisStr(a))
		}
		name := strings.ToUpper(argv[0])
		var res any
		switch {
		case name == "AUTH":
			if db.pass == "" {
				res = redisErr("ERR AUTH <password> called without any password configured for the default user. Are you sure your configuration is correct?")
			} else if argv[len(argv)-1] == db.pass {
				authed, res = true, "OK"
			} else {
				res = redisErr("WRONGPASS invalid username-password pair or user is disabled.")
			}
		case !authed && name != "HELLO" && name != "QUIT":
			res = redisErr("NOAUTH Authentication required.")
		case name == "QUIT":
			redisWrite(w, "OK")
			w.Flush()
			return
		case name == "SHUTDOWN":
			shutdown()
			return
		default:
			db.mu.Lock()
			res = redisExec(db, argv, version, port)
			db.mu.Unlock()
		}
		redisWrite(w, res)
		if w.Flush() != nil {
			return
		}
	}
}

func redisStr(a any) any {
	if b, ok := a.([]byte); ok {
		return string(b)
	}
	return a
}

// redisRead reads one value of RESP, or an inline command.
func redisRead(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return []any{}, nil
	}
	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return redisErr(line[1:]), nil
	case ':':
		n, _ := strconv.ParseInt(line[1:], 10, 64)
		return int(n), nil
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		if n > 512<<20 {
			return nil, errors.New("bulk too large")
		}
		b := make([]byte, n+2)
		if _, err := io.ReadFull(r, b); err != nil {
			return nil, err
		}
		return b[:n], nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, err
		}
		if n < 0 {
			return nil, nil
		}
		if n > 1<<20 {
			return nil, errors.New("array too large")
		}
		arr := make([]any, n)
		for i := range arr {
			if arr[i], err = redisRead(r); err != nil {
				return nil, err
			}
		}
		return arr, nil
	}
	// an inline command: PING, SET a b
	var arr []any
	for _, f := range splitInline(line) {
		arr = append(arr, []byte(f))
	}
	return arr, nil
}

// splitInline splits a command line like redis-cli does: words, "quoted"
// and 'quoted' strings.
func splitInline(s string) []string {
	var out []string
	var cur strings.Builder
	in, quote := false, byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case quote != 0:
			if c == '\\' && i+1 < len(s) && quote == '"' {
				i++
				switch s[i] {
				case 'n':
					cur.WriteByte('\n')
				case 't':
					cur.WriteByte('\t')
				default:
					cur.WriteByte(s[i])
				}
			} else if c == quote {
				quote = 0
			} else {
				cur.WriteByte(c)
			}
		case c == '"' || c == '\'':
			quote, in = c, true
		case c == ' ' || c == '\t':
			if in {
				out = append(out, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteByte(c)
			in = true
		}
	}
	if in {
		out = append(out, cur.String())
	}
	return out
}

func redisWrite(w *bufio.Writer, v any) {
	switch v := v.(type) {
	case nil:
		w.WriteString("$-1\r\n")
	case string:
		w.WriteString("+" + v + "\r\n")
	case redisErr:
		w.WriteString("-" + string(v) + "\r\n")
	case int:
		fmt.Fprintf(w, ":%d\r\n", v)
	case []byte:
		fmt.Fprintf(w, "$%d\r\n", len(v))
		w.Write(v)
		w.WriteString("\r\n")
	case []any:
		fmt.Fprintf(w, "*%d\r\n", len(v))
		for _, x := range v {
			redisWrite(w, x)
		}
	case nilArray:
		w.WriteString("*-1\r\n")
	}
}

type nilArray struct{}

func bulk(s string) any { return []byte(s) }

func bulks(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = bulk(s)
	}
	return out
}

func arity(argv []string, n int) redisErr {
	if len(argv) < n {
		return redisErr(fmt.Sprintf("ERR wrong number of arguments for '%s' command", strings.ToLower(argv[0])))
	}
	return ""
}

// redisExec runs one command. db.mu is held.
func redisExec(db *redisDB, argv []string, version, port string) any {
	name := strings.ToUpper(argv[0])
	need := map[string]int{
		"ECHO": 2, "GET": 2, "SET": 3, "SETNX": 3, "SETEX": 4, "GETSET": 3, "GETDEL": 2, "DEL": 2, "UNLINK": 2, "EXISTS": 2,
		"INCR": 2, "DECR": 2, "INCRBY": 3, "DECRBY": 3, "APPEND": 3, "STRLEN": 2, "MGET": 2, "MSET": 3,
		"EXPIRE": 3, "PEXPIRE": 3, "TTL": 2, "PTTL": 2, "PERSIST": 2, "TYPE": 2, "KEYS": 2, "RENAME": 3,
		"LPUSH": 3, "RPUSH": 3, "LPOP": 2, "RPOP": 2, "LRANGE": 4, "LLEN": 2, "LINDEX": 3, "LREM": 4,
		"HSET": 4, "HMSET": 4, "HGET": 3, "HMGET": 3, "HDEL": 3, "HGETALL": 2, "HKEYS": 2, "HVALS": 2, "HLEN": 2, "HEXISTS": 3, "HINCRBY": 4,
		"SADD": 3, "SREM": 3, "SMEMBERS": 2, "SISMEMBER": 3, "SCARD": 2,
	}
	if n, ok := need[name]; ok {
		if e := arity(argv, n); e != "" {
			return e
		}
	}
	key := ""
	if len(argv) > 1 {
		key = argv[1]
	}
	str := func(k string) (*redisVal, redisErr) {
		v := db.get(k)
		if v != nil && v.kind != "string" {
			return nil, wrongType
		}
		return v, ""
	}
	typed := func(k, kind string, create bool) (*redisVal, redisErr) {
		v := db.get(k)
		if v == nil {
			if !create {
				return nil, ""
			}
			v = &redisVal{kind: kind, hash: map[string]string{}, set: map[string]bool{}}
			db.keys[k] = v
		}
		if v.kind != kind {
			return nil, wrongType
		}
		return v, ""
	}
	incr := func(by int) any {
		v, e := str(key)
		if e != "" {
			return e
		}
		n := 0
		if v != nil {
			var err error
			if n, err = strconv.Atoi(v.str); err != nil {
				return redisErr("ERR value is not an integer or out of range")
			}
		} else {
			v = &redisVal{kind: "string"}
			db.keys[key] = v
		}
		n += by
		v.str = strconv.Itoa(n)
		return n
	}
	intArg := func(s string) (int, redisErr) {
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, "ERR value is not an integer or out of range"
		}
		return n, ""
	}
	switch name {
	case "PING":
		if len(argv) > 1 {
			return bulk(argv[1])
		}
		return "PONG"
	case "ECHO":
		return bulk(argv[1])
	case "HELLO":
		return []any{bulk("server"), bulk("redis"), bulk("version"), bulk(version), bulk("proto"), 2, bulk("id"), 1, bulk("mode"), bulk("standalone"), bulk("role"), bulk("master"), bulk("modules"), []any{}}
	case "SELECT", "CLIENT", "READONLY", "RESET":
		return "OK"
	case "COMMAND":
		return []any{}
	case "CONFIG":
		if len(argv) > 1 && strings.EqualFold(argv[1], "GET") {
			return []any{}
		}
		return "OK"
	case "DBSIZE":
		n := 0
		for k := range db.keys {
			if db.get(k) != nil {
				n++
			}
		}
		return n
	case "FLUSHALL", "FLUSHDB":
		db.keys = map[string]*redisVal{}
		return "OK"
	case "INFO":
		return bulk(fmt.Sprintf("# Server\r\nredis_version:%s\r\nredis_mode:standalone\r\nos:Linux 6.0.0-tabernetes wasm\r\narch_bits:64\r\nprocess_id:1\r\ntcp_port:%s\r\n\r\n# Clients\r\nconnected_clients:1\r\n\r\n# Keyspace\r\ndb0:keys=%d,expires=0,avg_ttl=0\r\n", version, port, len(db.keys)))
	case "GET":
		v, e := str(key)
		if e != "" {
			return e
		}
		if v == nil {
			return nil
		}
		return bulk(v.str)
	case "SET":
		var exp time.Time
		nx, xx, get := false, false, false
		for i := 3; i < len(argv); i++ {
			switch o := strings.ToUpper(argv[i]); o {
			case "NX":
				nx = true
			case "XX":
				xx = true
			case "GET":
				get = true
			case "KEEPTTL":
				if v := db.get(key); v != nil {
					exp = v.expire
				}
			case "EX", "PX":
				if i+1 >= len(argv) {
					return redisErr("ERR syntax error")
				}
				i++
				n, e := intArg(argv[i])
				if e != "" || n <= 0 {
					return redisErr("ERR invalid expire time in 'set' command")
				}
				unit := time.Second
				if o == "PX" {
					unit = time.Millisecond
				}
				exp = time.Now().Add(time.Duration(n) * unit)
			default:
				return redisErr("ERR syntax error")
			}
		}
		old := db.get(key)
		var prev any
		if get {
			if old != nil && old.kind != "string" {
				return wrongType
			}
			if old != nil {
				prev = bulk(old.str)
			}
		}
		if (nx && old != nil) || (xx && old == nil) {
			if get {
				return prev
			}
			return nil
		}
		db.keys[key] = &redisVal{kind: "string", str: argv[2], expire: exp}
		if get {
			return prev
		}
		return "OK"
	case "SETNX":
		if db.get(key) != nil {
			return 0
		}
		db.keys[key] = &redisVal{kind: "string", str: argv[2]}
		return 1
	case "SETEX":
		n, e := intArg(argv[2])
		if e != "" || n <= 0 {
			return redisErr("ERR invalid expire time in 'setex' command")
		}
		db.keys[key] = &redisVal{kind: "string", str: argv[3], expire: time.Now().Add(time.Duration(n) * time.Second)}
		return "OK"
	case "GETSET", "GETDEL":
		v, e := str(key)
		if e != "" {
			return e
		}
		if name == "GETSET" {
			db.keys[key] = &redisVal{kind: "string", str: argv[2]}
		} else {
			delete(db.keys, key)
		}
		if v == nil {
			return nil
		}
		return bulk(v.str)
	case "MGET":
		out := make([]any, 0, len(argv)-1)
		for _, k := range argv[1:] {
			if v := db.get(k); v != nil && v.kind == "string" {
				out = append(out, bulk(v.str))
			} else {
				out = append(out, nil)
			}
		}
		return out
	case "MSET":
		if len(argv)%2 != 1 {
			return arity(argv, len(argv)+1)
		}
		for i := 1; i+1 < len(argv); i += 2 {
			db.keys[argv[i]] = &redisVal{kind: "string", str: argv[i+1]}
		}
		return "OK"
	case "DEL", "UNLINK":
		n := 0
		for _, k := range argv[1:] {
			if db.get(k) != nil {
				delete(db.keys, k)
				n++
			}
		}
		return n
	case "EXISTS":
		n := 0
		for _, k := range argv[1:] {
			if db.get(k) != nil {
				n++
			}
		}
		return n
	case "INCR":
		return incr(1)
	case "DECR":
		return incr(-1)
	case "INCRBY", "DECRBY":
		n, e := intArg(argv[2])
		if e != "" {
			return e
		}
		if name == "DECRBY" {
			n = -n
		}
		return incr(n)
	case "APPEND":
		v, e := str(key)
		if e != "" {
			return e
		}
		if v == nil {
			v = &redisVal{kind: "string"}
			db.keys[key] = v
		}
		v.str += argv[2]
		return len(v.str)
	case "STRLEN":
		v, e := str(key)
		if e != "" {
			return e
		}
		if v == nil {
			return 0
		}
		return len(v.str)
	case "EXPIRE", "PEXPIRE":
		n, e := intArg(argv[2])
		if e != "" {
			return e
		}
		v := db.get(key)
		if v == nil {
			return 0
		}
		unit := time.Second
		if name == "PEXPIRE" {
			unit = time.Millisecond
		}
		v.expire = time.Now().Add(time.Duration(n) * unit)
		return 1
	case "PERSIST":
		v := db.get(key)
		if v == nil || v.expire.IsZero() {
			return 0
		}
		v.expire = time.Time{}
		return 1
	case "TTL", "PTTL":
		v := db.get(key)
		switch {
		case v == nil:
			return -2
		case v.expire.IsZero():
			return -1
		case name == "PTTL":
			return int(time.Until(v.expire).Milliseconds())
		}
		return int((time.Until(v.expire) + time.Second - 1) / time.Second)
	case "TYPE":
		if v := db.get(key); v != nil {
			return v.kind
		}
		return "none"
	case "KEYS":
		var out []string
		for k := range db.keys {
			if db.get(k) == nil {
				continue
			}
			if ok, _ := path.Match(argv[1], k); ok {
				out = append(out, k)
			}
		}
		sort.Strings(out)
		return bulks(out)
	case "RENAME":
		v := db.get(key)
		if v == nil {
			return redisErr("ERR no such key")
		}
		delete(db.keys, key)
		db.keys[argv[2]] = v
		return "OK"
	case "LPUSH", "RPUSH":
		v, e := typed(key, "list", true)
		if e != "" {
			return e
		}
		for _, x := range argv[2:] {
			if name == "LPUSH" {
				v.list = append([]string{x}, v.list...)
			} else {
				v.list = append(v.list, x)
			}
		}
		return len(v.list)
	case "LPOP", "RPOP":
		v, e := typed(key, "list", false)
		if e != "" {
			return e
		}
		if v == nil || len(v.list) == 0 {
			return nil
		}
		var x string
		if name == "LPOP" {
			x, v.list = v.list[0], v.list[1:]
		} else {
			x, v.list = v.list[len(v.list)-1], v.list[:len(v.list)-1]
		}
		if len(v.list) == 0 {
			delete(db.keys, key)
		}
		return bulk(x)
	case "LLEN":
		v, e := typed(key, "list", false)
		if e != "" {
			return e
		}
		if v == nil {
			return 0
		}
		return len(v.list)
	case "LRANGE":
		v, e := typed(key, "list", false)
		if e != "" {
			return e
		}
		start, e1 := intArg(argv[2])
		stop, e2 := intArg(argv[3])
		if e1 != "" || e2 != "" {
			return redisErr("ERR value is not an integer or out of range")
		}
		if v == nil {
			return []any{}
		}
		n := len(v.list)
		if start < 0 {
			start = max(n+start, 0)
		}
		if stop < 0 {
			stop = n + stop
		}
		stop = min(stop, n-1)
		if start > stop {
			return []any{}
		}
		return bulks(v.list[start : stop+1])
	case "LINDEX":
		v, e := typed(key, "list", false)
		if e != "" {
			return e
		}
		i, e := intArg(argv[2])
		if e != "" {
			return e
		}
		if v == nil {
			return nil
		}
		if i < 0 {
			i += len(v.list)
		}
		if i < 0 || i >= len(v.list) {
			return nil
		}
		return bulk(v.list[i])
	case "LREM":
		v, e := typed(key, "list", false)
		if e != "" {
			return e
		}
		count, e := intArg(argv[2])
		if e != "" {
			return e
		}
		if v == nil {
			return 0
		}
		removed := 0
		var keep []string
		for _, x := range v.list {
			if x == argv[3] && (count == 0 || removed < max(count, -count)) {
				removed++
				continue
			}
			keep = append(keep, x)
		}
		v.list = keep
		return removed
	case "HSET", "HMSET":
		if len(argv)%2 != 0 {
			return arity(argv, len(argv)+1)
		}
		v, e := typed(key, "hash", true)
		if e != "" {
			return e
		}
		n := 0
		for i := 2; i+1 < len(argv); i += 2 {
			if _, ok := v.hash[argv[i]]; !ok {
				n++
			}
			v.hash[argv[i]] = argv[i+1]
		}
		if name == "HMSET" {
			return "OK"
		}
		return n
	case "HGET":
		v, e := typed(key, "hash", false)
		if e != "" {
			return e
		}
		if v == nil {
			return nil
		}
		if x, ok := v.hash[argv[2]]; ok {
			return bulk(x)
		}
		return nil
	case "HMGET":
		v, e := typed(key, "hash", false)
		if e != "" {
			return e
		}
		var out []any
		for _, f := range argv[2:] {
			if x, ok := v.hashGet(f); ok {
				out = append(out, bulk(x))
			} else {
				out = append(out, nil)
			}
		}
		return out
	case "HDEL":
		v, e := typed(key, "hash", false)
		if e != "" {
			return e
		}
		n := 0
		for _, f := range argv[2:] {
			if _, ok := v.hashGet(f); ok {
				delete(v.hash, f)
				n++
			}
		}
		return n
	case "HGETALL", "HKEYS", "HVALS", "HLEN":
		v, e := typed(key, "hash", false)
		if e != "" {
			return e
		}
		var fields []string
		if v != nil {
			for f := range v.hash {
				fields = append(fields, f)
			}
		}
		sort.Strings(fields)
		var out []string
		for _, f := range fields {
			switch name {
			case "HGETALL":
				out = append(out, f, v.hash[f])
			case "HKEYS":
				out = append(out, f)
			case "HVALS":
				out = append(out, v.hash[f])
			}
		}
		if name == "HLEN" {
			return len(fields)
		}
		return bulks(out)
	case "HEXISTS":
		v, e := typed(key, "hash", false)
		if e != "" {
			return e
		}
		if _, ok := v.hashGet(argv[2]); ok {
			return 1
		}
		return 0
	case "HINCRBY":
		v, e := typed(key, "hash", true)
		if e != "" {
			return e
		}
		by, e := intArg(argv[3])
		if e != "" {
			return e
		}
		n := 0
		if x, ok := v.hash[argv[2]]; ok {
			var err error
			if n, err = strconv.Atoi(x); err != nil {
				return redisErr("ERR hash value is not an integer")
			}
		}
		n += by
		v.hash[argv[2]] = strconv.Itoa(n)
		return n
	case "SADD":
		v, e := typed(key, "set", true)
		if e != "" {
			return e
		}
		n := 0
		for _, m := range argv[2:] {
			if !v.set[m] {
				v.set[m] = true
				n++
			}
		}
		return n
	case "SREM":
		v, e := typed(key, "set", false)
		if e != "" {
			return e
		}
		n := 0
		if v != nil {
			for _, m := range argv[2:] {
				if v.set[m] {
					delete(v.set, m)
					n++
				}
			}
		}
		return n
	case "SMEMBERS", "SCARD":
		v, e := typed(key, "set", false)
		if e != "" {
			return e
		}
		var out []string
		if v != nil {
			for m := range v.set {
				out = append(out, m)
			}
		}
		if name == "SCARD" {
			return len(out)
		}
		sort.Strings(out)
		return bulks(out)
	case "SISMEMBER":
		v, e := typed(key, "set", false)
		if e != "" {
			return e
		}
		if v != nil && v.set[argv[2]] {
			return 1
		}
		return 0
	}
	quoted := make([]string, 0, len(argv)-1)
	for _, a := range argv[1:] {
		quoted = append(quoted, "'"+a+"'")
	}
	return redisErr(fmt.Sprintf("ERR unknown command '%s', with args beginning with: %s", argv[0], strings.Join(quoted, " ")))
}

func (v *redisVal) hashGet(f string) (string, bool) {
	if v == nil {
		return "", false
	}
	x, ok := v.hash[f]
	return x, ok
}

// redisCliProg is redis-cli: a command from the arguments, or commands from
// stdin with a prompt (kubectl exec -it ... redis-cli).
func redisCliProg(ctx context.Context, hc interp.HandlerContext, args []string, _ interp.ExecHandlerFunc) error {
	host, port, pass := "127.0.0.1", "6379", ""
	raw, noRaw := false, false
	var cmd []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		next := func() string {
			if i+1 < len(args) {
				i++
				return args[i]
			}
			return ""
		}
		switch {
		case len(cmd) > 0:
			cmd = append(cmd, a)
		case a == "-h":
			host = next()
		case a == "-p":
			port = next()
		case a == "-a":
			pass = next()
		case a == "-n":
			next()
		case a == "--raw":
			raw = true
		case a == "--no-raw":
			noRaw = true
		case a == "--no-auth-warning":
		case a == "-u":
			if u, err := parseRedisURL(next()); err == nil {
				host, port, pass = u[0], u[1], u[2]
			}
		case a == "-v" || a == "--version":
			fmt.Fprintln(hc.Stdout, "redis-cli 8.2.2")
			return nil
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(hc.Stderr, "Unrecognized option or bad number of args for: '%s'\n", a)
			return interp.ExitStatus(1)
		default:
			cmd = append(cmd, a)
		}
	}
	if pass == "" {
		pass = getenv(hc, "REDISCLI_AUTH")
	} else {
		fmt.Fprintln(hc.Stderr, "Warning: Using a password with '-a' or '-u' option on the command line interface may not be safe.")
	}
	addr := net.JoinHostPort(host, port)
	conn, err := dialCluster(ctx, "tcp", addr)
	if err != nil {
		fmt.Fprintf(hc.Stderr, "Could not connect to Redis at %s: Connection refused\n", addr)
		if len(cmd) == 0 {
			fmt.Fprintln(hc.Stdout, "not connected> ")
		}
		return interp.ExitStatus(1)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	r := bufio.NewReader(conn)
	w := bufio.NewWriter(conn)
	call := func(argv []string) (any, error) {
		arr := make([]any, len(argv))
		for i, a := range argv {
			arr[i] = []byte(a)
		}
		redisWrite(w, arr)
		if err := w.Flush(); err != nil {
			return nil, err
		}
		return redisRead(r)
	}
	if pass != "" {
		if res, err := call([]string{"AUTH", pass}); err == nil {
			if e, ok := res.(redisErr); ok {
				fmt.Fprintf(hc.Stderr, "AUTH failed: %s\n", strings.TrimPrefix(string(e), "WRONGPASS "))
			}
		}
	}
	if len(cmd) > 0 {
		res, err := call(cmd)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "Error: Server closed the connection")
			return interp.ExitStatus(1)
		}
		// Without a terminal, redis-cli prints raw values.
		fmt.Fprint(hc.Stdout, redisFormat(res, raw || !noRaw, ""))
		return nil
	}
	// Commands from stdin: with a terminal (TERM is set by exec -t, or the
	// container has tty: true) a prompt and typed replies, else raw replies.
	tty := getenv(hc, "TERM") != ""
	if pe := procOf(ctx); pe != nil && pe.c != nil && pe.c.spec.TTY {
		tty = true
	}
	prompt := ""
	if tty {
		prompt = addr + "> "
	}
	in := bufio.NewScanner(stdinOf(hc))
	fmt.Fprint(hc.Stdout, prompt)
	for in.Scan() {
		argv := splitInline(strings.TrimSpace(in.Text()))
		if len(argv) == 0 {
			fmt.Fprint(hc.Stdout, prompt)
			continue
		}
		if q := strings.ToLower(argv[0]); q == "quit" || q == "exit" {
			return nil
		}
		res, err := call(argv)
		if err != nil {
			fmt.Fprintln(hc.Stderr, "Error: Server closed the connection")
			return interp.ExitStatus(1)
		}
		fmt.Fprint(hc.Stdout, redisFormat(res, raw || !tty, ""))
		fmt.Fprint(hc.Stdout, prompt)
	}
	return nil
}

func parseRedisURL(s string) ([3]string, error) {
	s, ok := strings.CutPrefix(s, "redis://")
	if !ok {
		return [3]string{}, errors.New("not a redis URL")
	}
	var pass string
	if at := strings.LastIndexByte(s, '@'); at >= 0 {
		_, pass, _ = strings.Cut(s[:at], ":")
		s = s[at+1:]
	}
	s, _, _ = strings.Cut(s, "/")
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		host, port = s, "6379"
	}
	return [3]string{host, port, pass}, nil
}

// redisFormat prints a reply like redis-cli: raw without a terminal, else
// with types: "value", (integer) 1, (nil), 1) ...
func redisFormat(v any, raw bool, indent string) string {
	switch v := v.(type) {
	case nil:
		if raw {
			return "\n"
		}
		return "(nil)\n"
	case string:
		return v + "\n"
	case redisErr:
		if raw {
			return string(v) + "\n"
		}
		return "(error) " + string(v) + "\n"
	case int:
		if raw {
			return strconv.Itoa(v) + "\n"
		}
		return fmt.Sprintf("(integer) %d\n", v)
	case []byte:
		if raw {
			return string(v) + "\n"
		}
		return strconv.Quote(string(v)) + "\n"
	case []any:
		if len(v) == 0 {
			if raw {
				return ""
			}
			return "(empty array)\n"
		}
		var b strings.Builder
		width := len(strconv.Itoa(len(v)))
		for i, x := range v {
			if raw {
				b.WriteString(redisFormat(x, true, ""))
				continue
			}
			pre := fmt.Sprintf("%*d) ", width, i+1)
			if i > 0 {
				b.WriteString(indent)
			}
			b.WriteString(pre + redisFormat(x, false, indent+strings.Repeat(" ", len(pre))))
		}
		return b.String()
	}
	return fmt.Sprint(v) + "\n"
}
