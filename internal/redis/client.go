package redis

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // Redis addresses scripts by SHA-1; not a security choice
	"encoding/hex"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options configures a Client.
type Options struct {
	// Addr is "host:port" or "unix:/path/to/socket".
	Addr string
	// Timeout bounds a single command, dial included.
	Timeout time.Duration
	// PoolSize caps idle connections kept for reuse.
	PoolSize int
}

// Client is a small pooled RESP client, safe for concurrent use.
//
// Connections are pooled because the token bucket is taken once per probe: a
// fresh TCP handshake per command would put a round trip on the hot path and
// make the limiter itself the bottleneck it exists to avoid.
type Client struct {
	network string
	address string
	timeout time.Duration

	mu   sync.Mutex
	idle []*conn
	cap  int
}

type conn struct {
	c net.Conn
	r *bufio.Reader
}

// New returns a Client. It opens nothing until the first command.
func New(opts Options) *Client {
	addr := opts.Addr
	if addr == "" {
		addr = "127.0.0.1:6379"
	}
	network := "tcp"
	if path, ok := strings.CutPrefix(addr, "unix:"); ok {
		network, addr = "unix", path
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	size := opts.PoolSize
	if size <= 0 {
		size = 8
	}
	return &Client{network: network, address: addr, timeout: timeout, cap: size}
}

func (cl *Client) get(ctx context.Context) (*conn, error) {
	cl.mu.Lock()
	if n := len(cl.idle); n > 0 {
		c := cl.idle[n-1]
		cl.idle = cl.idle[:n-1]
		cl.mu.Unlock()
		return c, nil
	}
	cl.mu.Unlock()

	d := net.Dialer{Timeout: cl.timeout}
	c, err := d.DialContext(ctx, cl.network, cl.address)
	if err != nil {
		return nil, err
	}
	return &conn{c: c, r: bufio.NewReader(c)}, nil
}

// put returns a healthy connection to the pool; a broken one is discarded, so
// a half-closed socket is never handed to the next caller.
func (cl *Client) put(c *conn, err error) {
	var protoErr *Error
	if err != nil && !errors.As(err, &protoErr) {
		_ = c.c.Close()
		return
	}
	cl.mu.Lock()
	defer cl.mu.Unlock()
	if len(cl.idle) >= cl.cap {
		_ = c.c.Close()
		return
	}
	cl.idle = append(cl.idle, c)
}

// Close releases every pooled connection.
func (cl *Client) Close() error {
	cl.mu.Lock()
	defer cl.mu.Unlock()
	for _, c := range cl.idle {
		_ = c.c.Close()
	}
	cl.idle = nil
	return nil
}

// Do runs one command and returns the decoded reply.
func (cl *Client) Do(ctx context.Context, args ...string) (any, error) {
	c, err := cl.get(ctx)
	if err != nil {
		return nil, err
	}

	deadline := time.Now().Add(cl.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := c.c.SetDeadline(deadline); err != nil {
		_ = c.c.Close()
		return nil, err
	}

	var b strings.Builder
	encode(&b, args)
	if _, err := c.c.Write([]byte(b.String())); err != nil {
		_ = c.c.Close()
		return nil, err
	}

	reply, err := readReply(c.r)
	cl.put(c, err)
	return reply, err
}

// Ping is the readiness check.
func (cl *Client) Ping(ctx context.Context) error {
	_, err := cl.Do(ctx, "PING")
	return err
}

// Get returns the value and whether the key existed.
func (cl *Client) Get(ctx context.Context, key string) (string, bool, error) {
	v, err := cl.Do(ctx, "GET", key)
	if err != nil || v == nil {
		return "", false, err
	}
	s, ok := v.(string)
	if !ok {
		return "", false, errors.New("redis: GET returned a non-string reply")
	}
	return s, true, nil
}

// Set writes a key with no expiry.
func (cl *Client) Set(ctx context.Context, key, val string) error {
	_, err := cl.Do(ctx, "SET", key, val)
	return err
}

// Script is a Lua script addressed by its SHA-1, as Redis does.
type Script struct {
	body string
	sha  string
}

// NewScript prepares body for EVALSHA.
func NewScript(body string) *Script {
	sum := sha1.Sum([]byte(body)) //nolint:gosec // Redis's own addressing scheme
	return &Script{body: body, sha: hex.EncodeToString(sum[:])}
}

// Run executes the script, sending the body only when the server has not
// cached it. Shipping a kilobyte of Lua on every probe would defeat the point
// of keeping take+refill to one round trip.
func (cl *Client) Run(ctx context.Context, s *Script, keys, args []string) (any, error) {
	call := func(verb, script string) (any, error) {
		argv := make([]string, 0, 3+len(keys)+len(args))
		argv = append(argv, verb, script, strconv.Itoa(len(keys)))
		argv = append(argv, keys...)
		argv = append(argv, args...)
		return cl.Do(ctx, argv...)
	}

	out, err := call("EVALSHA", s.sha)
	var rerr *Error
	if errors.As(err, &rerr) && strings.HasPrefix(rerr.Msg, "NOSCRIPT") {
		return call("EVAL", s.body)
	}
	return out, err
}
