package redis

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestReadReply(t *testing.T) {
	for name, tc := range map[string]struct {
		wire string
		want any
	}{
		"simple string": {"+OK\r\n", "OK"},
		"integer":       {":42\r\n", int64(42)},
		"bulk":          {"$5\r\nhello\r\n", "hello"},
		"null bulk":     {"$-1\r\n", nil},
		"empty bulk":    {"$0\r\n\r\n", ""},
		"array":         {"*2\r\n:1\r\n$3\r\nabc\r\n", []any{int64(1), "abc"}},
		"null array":    {"*-1\r\n", nil},
		"nested array":  {"*1\r\n*2\r\n:1\r\n:2\r\n", []any{[]any{int64(1), int64(2)}}},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readReply(bufio.NewReader(strings.NewReader(tc.wire)))
			if err != nil {
				t.Fatalf("readReply: %v", err)
			}
			if !equal(got, tc.want) {
				t.Errorf("readReply(%q) = %#v, want %#v", tc.wire, got, tc.want)
			}
		})
	}
}

// A rejection by Redis is a different thing from a broken socket: the first is
// our bug, the second means fail closed.
func TestReadReplyErrorIsTyped(t *testing.T) {
	_, err := readReply(bufio.NewReader(strings.NewReader("-NOSCRIPT No matching script\r\n")))
	var rerr *Error
	if !errors.As(err, &rerr) {
		t.Fatalf("error is %T, want *redis.Error", err)
	}
	if !strings.HasPrefix(rerr.Msg, "NOSCRIPT") {
		t.Errorf("Msg = %q", rerr.Msg)
	}
}

func equal(a, b any) bool {
	x, aok := a.([]any)
	y, bok := b.([]any)
	if aok != bok {
		return false
	}
	if !aok {
		return a == b
	}
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if !equal(x[i], y[i]) {
			return false
		}
	}
	return true
}

func TestEncode(t *testing.T) {
	var b strings.Builder
	encode(&b, []string{"GET", "k"})
	if got, want := b.String(), "*2\r\n$3\r\nGET\r\n$1\r\nk\r\n"; got != want {
		t.Errorf("encode = %q, want %q", got, want)
	}
}

// fakeServer speaks just enough RESP to exercise the client, and records every
// command it saw.
type fakeServer struct {
	ln    net.Listener
	mu    sync.Mutex
	seen  [][]string
	reply func(args []string) string
}

func newFakeServer(t *testing.T, reply func([]string) string) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, reply: reply}
	go f.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return f
}

func (f *fakeServer) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer func() { _ = c.Close() }()
			r := bufio.NewReader(c)
			for {
				args, err := readCommand(r)
				if err != nil {
					return
				}
				f.mu.Lock()
				f.seen = append(f.seen, args)
				f.mu.Unlock()
				if _, err := c.Write([]byte(f.reply(args))); err != nil {
					return
				}
			}
		}()
	}
}

func (f *fakeServer) commands() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.seen))
	copy(out, f.seen)
	return out
}

func readCommand(r *bufio.Reader) ([]string, error) {
	v, err := readReply(r)
	if err != nil {
		return nil, err
	}
	arr, ok := v.([]any)
	if !ok {
		return nil, errors.New("not a command")
	}
	out := make([]string, 0, len(arr))
	for _, a := range arr {
		s, _ := a.(string)
		out = append(out, s)
	}
	return out, nil
}

func TestClientGetSetPing(t *testing.T) {
	f := newFakeServer(t, func(args []string) string {
		switch args[0] {
		case "PING":
			return "+PONG\r\n"
		case "SET":
			return "+OK\r\n"
		case "GET":
			if args[1] == "missing" {
				return "$-1\r\n"
			}
			return "$5\r\nvalue\r\n"
		}
		return "-ERR unknown\r\n"
	})
	cl := New(Options{Addr: f.ln.Addr().String(), Timeout: 2 * time.Second})
	defer func() { _ = cl.Close() }()
	ctx := t.Context()

	if err := cl.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if err := cl.Set(ctx, "k", "v"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	v, ok, err := cl.Get(ctx, "k")
	if err != nil || !ok || v != "value" {
		t.Errorf("Get = %q,%v,%v", v, ok, err)
	}
	if _, ok, err := cl.Get(ctx, "missing"); err != nil || ok {
		t.Errorf("missing key reported as present: %v %v", ok, err)
	}
}

// Take+refill runs once per probe, so a fresh handshake per command would put a
// round trip on the hot path.
func TestClientReusesConnections(t *testing.T) {
	var conns int
	var mu sync.Mutex
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns++
			mu.Unlock()
			go func() {
				defer func() { _ = c.Close() }()
				r := bufio.NewReader(c)
				for {
					if _, err := readCommand(r); err != nil {
						return
					}
					if _, err := c.Write([]byte("+PONG\r\n")); err != nil {
						return
					}
				}
			}()
		}
	}()

	cl := New(Options{Addr: ln.Addr().String()})
	defer func() { _ = cl.Close() }()
	for range 20 {
		if err := cl.Ping(t.Context()); err != nil {
			t.Fatalf("Ping: %v", err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if conns != 1 {
		t.Errorf("opened %d connections for 20 sequential commands, want 1", conns)
	}
}

// Shipping a kilobyte of Lua on every probe would defeat keeping take+refill to
// one round trip, so the body goes only when the server has not cached it.
func TestRunPrefersEVALSHAAndFallsBack(t *testing.T) {
	var served int
	f := newFakeServer(t, func(args []string) string {
		if args[0] == "EVALSHA" {
			served++
			if served == 1 {
				return "-NOSCRIPT No matching script\r\n"
			}
		}
		return ":7\r\n"
	})
	cl := New(Options{Addr: f.ln.Addr().String()})
	defer func() { _ = cl.Close() }()

	s := NewScript("return 7")
	for range 2 {
		if _, err := cl.Run(t.Context(), s, []string{"k"}, []string{"a"}); err != nil {
			t.Fatalf("Run: %v", err)
		}
	}

	var verbs []string
	for _, c := range f.commands() {
		verbs = append(verbs, c[0])
	}
	want := []string{"EVALSHA", "EVAL", "EVALSHA"}
	if strings.Join(verbs, ",") != strings.Join(want, ",") {
		t.Errorf("verbs = %v, want %v — the body must be sent only once", verbs, want)
	}
}

func TestNewParsesUnixAddr(t *testing.T) {
	cl := New(Options{Addr: "unix:/run/redis/redis-server.sock"})
	if cl.network != "unix" || cl.address != "/run/redis/redis-server.sock" {
		t.Errorf("network,address = %q,%q", cl.network, cl.address)
	}
	if tcp := New(Options{Addr: "127.0.0.1:6379"}); tcp.network != "tcp" {
		t.Errorf("network = %q, want tcp", tcp.network)
	}
}

func TestDoFailsWhenServerIsGone(t *testing.T) {
	cl := New(Options{Addr: "127.0.0.1:1", Timeout: 200 * time.Millisecond})
	defer func() { _ = cl.Close() }()
	if err := cl.Ping(context.Background()); err == nil {
		t.Error("Ping succeeded against a dead port")
	}
}
