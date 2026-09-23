package redis

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// A protocol-level rejection keeps the connection pooled (Redis is fine, the
// command was wrong); anything else discards it, so a half-read socket is never
// handed to the next caller (plan 027).
func TestPoolKeepsConnectionsAfterProtocolErrorsOnly(t *testing.T) {
	f := newFakeServer(t, func(args []string) string {
		switch args[0] {
		case "BAD":
			return "-ERR unknown command\r\n"
		case "HUGE":
			return "*1125899906842624\r\n"
		}
		return "+OK\r\n"
	})
	cl := New(Options{Addr: f.ln.Addr().String(), PoolSize: 1})
	defer func() { _ = cl.Close() }()
	ctx := t.Context()

	_, err := cl.Do(ctx, "BAD")
	var rerr *Error
	if !errors.As(err, &rerr) || !strings.Contains(rerr.Error(), "redis: ERR") {
		t.Fatalf("BAD = %v, want a *redis.Error", err)
	}
	if len(cl.idle) != 1 {
		t.Errorf("idle = %d after a protocol error, want the connection kept", len(cl.idle))
	}
	// A hostile length is an error, not a panic, and the connection is dropped:
	// the stream is no longer at a reply boundary (fix 2, end to end).
	if _, err := cl.Do(ctx, "HUGE"); err == nil || errors.As(err, &rerr) {
		t.Fatalf("HUGE = %v, want a transport-class error", err)
	}
	if len(cl.idle) != 0 {
		t.Errorf("idle = %d after a broken reply, want the connection discarded", len(cl.idle))
	}
	if err := cl.Ping(ctx); err != nil {
		t.Errorf("the client did not recover with a fresh connection: %v", err)
	}
}

// The pool never holds more than its cap.
func TestPoolIsCapped(t *testing.T) {
	f := newFakeServer(t, func([]string) string { return "+PONG\r\n" })
	cl := New(Options{Addr: f.ln.Addr().String(), PoolSize: 1})
	defer func() { _ = cl.Close() }()
	a, err := cl.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	b, err := cl.get(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cl.put(a, nil)
	cl.put(b, nil)
	if len(cl.idle) != 1 {
		t.Errorf("idle = %d, cap 1", len(cl.idle))
	}
}

// GET of something that is not a bulk string is an error, not a silent "".
func TestGetRejectsNonStringReply(t *testing.T) {
	f := newFakeServer(t, func([]string) string { return ":7\r\n" })
	cl := New(Options{Addr: f.ln.Addr().String()})
	defer func() { _ = cl.Close() }()
	if _, _, err := cl.Get(t.Context(), "k"); err == nil {
		t.Error("GET returning an integer was accepted")
	}
}

// A server that accepts and never answers is bounded by the client timeout.
func TestDoTimesOutOnASilentServer(t *testing.T) {
	var lc net.ListenConfig
	ln, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer func() { _ = c.Close() }()
			time.Sleep(2 * time.Second)
		}
	}()
	cl := New(Options{Addr: ln.Addr().String(), Timeout: 100 * time.Millisecond})
	defer func() { _ = cl.Close() }()
	start := time.Now()
	if err := cl.Ping(context.Background()); err == nil {
		t.Fatal("Ping against a silent server succeeded")
	}
	if time.Since(start) > time.Second {
		t.Errorf("took %s, want about the 100ms timeout", time.Since(start))
	}
}

func TestNewDefaults(t *testing.T) {
	cl := New(Options{})
	if cl.address != "127.0.0.1:6379" || cl.timeout != 2*time.Second || cl.cap != 8 {
		t.Errorf("defaults = %q %s %d", cl.address, cl.timeout, cl.cap)
	}
}
