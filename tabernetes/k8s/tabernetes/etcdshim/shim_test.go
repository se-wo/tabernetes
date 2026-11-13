package etcdshim

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

func TestWatch(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(l)
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{l.Addr().String()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := c.Put(ctx, "/a/x", "1")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("put rev %d", r.Header.Revision)
	w1 := c.Watch(ctx, "/a/", clientv3.WithPrefix(), clientv3.WithRev(r.Header.Revision+1), clientv3.WithPrevKV(), clientv3.WithProgressNotify())
	w2 := c.Watch(ctx, "/b/", clientv3.WithPrefix(), clientv3.WithPrevKV())
	time.Sleep(100 * time.Millisecond)
	c.Put(ctx, "/a/y", "2")
	c.Put(ctx, "/b/y", "2")
	for _, w := range []clientv3.WatchChan{w1, w2} {
		select {
		case resp := <-w:
			t.Logf("got %v err=%v", resp.Events, resp.Err())
			if len(resp.Events) != 1 {
				t.Fatalf("expected one event, got %d", len(resp.Events))
			}
		case <-ctx.Done():
			t.Fatal("timeout")
		}
	}
	if err := c.RequestProgress(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case resp := <-w1:
		t.Logf("progress: %v rev=%d", resp.IsProgressNotify(), resp.Header.Revision)
	case <-ctx.Done():
		t.Fatal("timeout")
	}
	tx, err := c.Txn(ctx).If(clientv3.Compare(clientv3.ModRevision("/a/x"), "=", r.Header.Revision)).Then(clientv3.OpPut("/a/x", "3")).Else(clientv3.OpGet("/a/x")).Commit()
	t.Logf("txn ok=%v err=%v", tx.Succeeded, err)
}

func TestLogfOnCompact(t *testing.T) {
	type entry struct {
		level, msg string
		kv         []any
	}
	logged := make(chan entry, 10)
	Logf = func(level, msg string, kv ...any) { logged <- entry{level, msg, kv} }
	defer func() { Logf = nil }()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go Serve(l)
	c, err := clientv3.New(clientv3.Config{Endpoints: []string{l.Addr().String()}, DialTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r, err := c.Put(ctx, "/a", "1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Compact(ctx, r.Header.Revision); err != nil {
		t.Fatal(err)
	}
	for {
		select {
		case e := <-logged:
			if e.msg != "finished scheduled compaction" {
				continue
			}
			if len(e.kv) < 2 || e.kv[0] != "compact-revision" || e.kv[1] != r.Header.Revision {
				t.Errorf("compaction entry %v", e.kv)
			}
			return
		case <-ctx.Done():
			t.Fatal("no log entry for the compaction")
		}
	}
}

func TestStoredBytes(t *testing.T) {
	before := StoredBytes()
	s := newStore()
	check := func(step string) {
		t.Helper()
		s.mu.Lock()
		defer s.mu.Unlock()
		if want := s.heldBytes(); s.bytes != want {
			t.Fatalf("%s: counter %d, walk %d", step, s.bytes, want)
		}
		if got := StoredBytes() - before; got != s.bytes {
			t.Fatalf("%s: StoredBytes grew by %d, store holds %d", step, got, s.bytes)
		}
	}
	write := func(f func()) {
		s.mu.Lock()
		s.rev++
		f()
		s.mu.Unlock()
	}
	check("empty")
	for i := range 20 {
		write(func() { s.put(s.rev, []byte(fmt.Sprintf("/k/%d", i%7)), bytes.Repeat([]byte("v"), 100*i), 0) })
	}
	check("puts")
	write(func() { s.del(s.rev, "/k/3") })
	write(func() { s.del(s.rev, "/k/4") })
	write(func() { s.put(s.rev, []byte("/k/4"), []byte("again"), 0) })
	check("deletes")
	s.mu.Lock()
	full := s.bytes
	s.compact(s.rev - 1)
	s.mu.Unlock()
	check("compact")
	s.mu.Lock()
	if s.bytes >= full {
		t.Errorf("compaction freed nothing: %d -> %d", full, s.bytes)
	}
	s.compact(s.rev)
	s.mu.Unlock()
	check("compact all")
}
