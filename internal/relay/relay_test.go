package relay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

type fakeResolver struct {
	hosts []string
	addrs []netip.Addr
	err   error
}

func (f fakeResolver) MX(context.Context, string) ([]string, error) { return f.hosts, f.err }
func (f fakeResolver) Resolve(context.Context, string) ([]netip.Addr, error) {
	return f.addrs, f.err
}

type fakePacer struct {
	acquireErr error
	throttled  []bool
}

func (f *fakePacer) Acquire(context.Context, string, string) error { return f.acquireErr }
func (f *fakePacer) Observe(_ context.Context, _ string, t bool) {
	f.throttled = append(f.throttled, t)
}

type fakeSuppression struct {
	hit       bool
	err       error
	stale     bool
	enforcing bool
}

func (f fakeSuppression) Suppressed(context.Context, string) (bool, string, error) {
	return f.hit, "suppressed by address", f.err
}
func (f fakeSuppression) Enforcing() bool            { return f.enforcing }
func (f fakeSuppression) Stale(context.Context) bool { return f.stale }

type fakeHealth struct{ burned bool }

func (f fakeHealth) Burned() (bool, string) { return f.burned, "zen.spamhaus.org" }

type dialerFunc func(ctx context.Context, network, address string) (net.Conn, error)

func (d dialerFunc) DialContext(ctx context.Context, n, a string) (net.Conn, error) {
	return d(ctx, n, a)
}

func testRelay(t *testing.T, opts Options) (*Relay, *fakeStore) {
	t.Helper()
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	signer, err := NewSigner("datascoutmail.com", "s1",
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	store := newFakeStore()
	if opts.Queue == nil {
		opts.Queue = NewQueue(store, 5)
	}
	opts.Signer = signer
	if opts.Helo == "" {
		opts.Helo = "mail.datascoutmail.com"
	}
	if opts.ReturnPath == nil {
		opts.ReturnPath = func(id string) string { return "bounces+" + id + "@datascoutmail.com" }
	}
	if opts.Pacer == nil {
		opts.Pacer = &fakePacer{}
	}
	return New(opts), store
}

func aMessage() Message {
	return Message{From: "noreply@datascoutmail.com", To: "someone@example.com",
		Subject: "Reset your password", Text: "Hello.\n"}
}

// Accepted means durable. A caller told "queued" and then losing the message to
// a restart is the failure this queue exists to prevent.
func TestAcceptQueuesASignedMessage(t *testing.T) {
	r, store := testRelay(t, Options{})
	id, err := r.Accept(t.Context(), aMessage())
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if !strings.HasSuffix(id, "@datascoutmail.com") {
		t.Errorf("message id %q is not addressable at the signing domain", id)
	}
	item, ok, err := NewQueue(store, 5).Next(t.Context())
	if err != nil || !ok {
		t.Fatalf("nothing survived to a second Queue: %v %v", ok, err)
	}
	if !strings.HasPrefix(string(item.Data), "DKIM-Signature: ") {
		t.Error("the queued bytes are not signed")
	}
	if !strings.HasPrefix(item.MailFrom, "bounces+") {
		t.Errorf("MailFrom = %q, want a VERP return path", item.MailFrom)
	}
}

// The difference from the verify path, and the reason it is written down in
// SECURITY.md: sending cannot be taken back, so an unreadable or stale list
// stops it rather than being noted and continued.
func TestSuppressionFailsClosedForSending(t *testing.T) {
	for name, s := range map[string]fakeSuppression{
		"a hit":              {enforcing: true, hit: true},
		"an unreadable list": {enforcing: true, err: errors.New("redis is gone")},
		"a stale list":       {enforcing: true, stale: true},
	} {
		r, _ := testRelay(t, Options{Suppress: s})
		if _, err := r.Accept(t.Context(), aMessage()); err == nil {
			t.Errorf("%s: Accept succeeded — the message would have been sent", name)
		}
	}
	// And with enforcement off it is not this service's call to make.
	r, _ := testRelay(t, Options{Suppress: fakeSuppression{enforcing: false, hit: true}})
	if _, err := r.Accept(t.Context(), aMessage()); err != nil {
		t.Errorf("with enforcement off, Accept should proceed: %v", err)
	}
}

// A listed IP stops sending. Verification keeps running — it asks questions
// rather than delivering — but every message from a listed address makes the
// listing harder to shake.
func TestABurnedIPStopsSendingEntirely(t *testing.T) {
	r, _ := testRelay(t, Options{Health: fakeHealth{burned: true}})
	if _, err := r.Accept(t.Context(), aMessage()); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	sent, err := r.DeliverNext(t.Context())
	if !errors.Is(err, ErrNotSending) {
		t.Fatalf("DeliverNext = %v, %v — want ErrNotSending", sent, err)
	}
}

// A message can wait hours in this queue. An erasure that arrives meanwhile
// must win, so the check is repeated at send time and not trusted from accept.
func TestSuppressionIsRecheckedAtSendTime(t *testing.T) {
	suppress := &mutableSuppression{enforcing: true}
	r, _ := testRelay(t, Options{
		Suppress: suppress,
		Resolver: fakeResolver{hosts: []string{"mx.example.com"}},
	})
	if _, err := r.Accept(t.Context(), aMessage()); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	suppress.hit = true // the request arrives while the message waits

	did, err := r.DeliverNext(t.Context())
	if err != nil || !did {
		t.Fatalf("DeliverNext = %v, %v", did, err)
	}
	if _, ok, _ := r.opts.Queue.Next(t.Context()); ok {
		t.Error("the message is still queued — it must be dropped, not retried")
	}
}

type mutableSuppression struct {
	hit       bool
	enforcing bool
}

func (m *mutableSuppression) Suppressed(context.Context, string) (bool, string, error) {
	return m.hit, "suppressed", nil
}
func (m *mutableSuppression) Enforcing() bool            { return m.enforcing }
func (m *mutableSuppression) Stale(context.Context) bool { return false }

// A transient failure is retried and a permanent one is not sent again; both
// leave the pacer told the truth about which it was.
func TestDeliveryOutcomesDriveTheQueueAndThePacer(t *testing.T) {
	for _, c := range []struct {
		name      string
		reply     string
		throttled bool
	}{
		{"greylisted", "451 4.7.1 Try again later", true},
		{"rejected", "550 5.1.1 No such user", false},
	} {
		pacer := &fakePacer{}
		conn, _ := fakeMX(t, func(cmd string) string {
			if strings.HasPrefix(cmd, "RCPT") {
				return c.reply
			}
			return plainMX(cmd)
		})
		r, _ := testRelay(t, Options{
			Pacer:    pacer,
			Resolver: fakeResolver{hosts: []string{"mx.example.com"}, addrs: []netip.Addr{netip.MustParseAddr("203.0.113.9")}},
			Dialer:   dialerFunc(func(context.Context, string, string) (net.Conn, error) { return conn, nil }),
			Timeout:  3 * time.Second,
		})
		if _, err := r.Accept(t.Context(), aMessage()); err != nil {
			t.Fatalf("%s: Accept: %v", c.name, err)
		}
		if _, err := r.DeliverNext(t.Context()); err != nil {
			t.Fatalf("%s: DeliverNext: %v", c.name, err)
		}
		if len(pacer.throttled) != 1 || pacer.throttled[0] != c.throttled {
			t.Errorf("%s: pacer told throttled=%v, want %v", c.name, pacer.throttled, c.throttled)
		}
	}
}

// Sending and verification share one pacer on purpose, so a refused budget
// defers rather than sends unpaced (invariant 5 applies to both legs).
func TestNoBudgetDefersRatherThanSends(t *testing.T) {
	pacer := &fakePacer{acquireErr: errors.New("redis unreachable")}
	dialled := false
	r, _ := testRelay(t, Options{
		Pacer:    pacer,
		Resolver: fakeResolver{hosts: []string{"mx.example.com"}},
		Dialer: dialerFunc(func(context.Context, string, string) (net.Conn, error) {
			dialled = true
			return nil, errors.New("should never be reached")
		}),
	})
	if _, err := r.Accept(t.Context(), aMessage()); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if _, err := r.DeliverNext(t.Context()); err != nil {
		t.Fatalf("DeliverNext: %v", err)
	}
	if dialled {
		t.Fatal("a socket was opened without a budget")
	}
}
