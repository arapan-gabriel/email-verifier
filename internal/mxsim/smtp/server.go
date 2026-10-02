// Package smtp implements just enough of RFC 5321 to be lied to convincingly.
// It is deliberately hand-rolled rather than built on a well-behaved SMTP
// library: half the value of the simulator is protocol misbehaviour (banner
// tarpits, mid-command hangups, out-of-sequence 503s) that a correct library
// exists to prevent.
package smtp

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/arapan-gabriel/email-verifier/internal/mxsim/policy"
)

const maxLineBytes = 4096

type Server struct {
	eng *policy.Engine
	log *slog.Logger

	mu    sync.Mutex
	lns   []net.Listener
	wg    sync.WaitGroup
	close bool
}

func New(eng *policy.Engine, log *slog.Logger) *Server {
	return &Server{eng: eng, log: log.With("profile", eng.Name())}
}

func (s *Server) Engine() *policy.Engine { return s.eng }

// Serve accepts connections until ctx is cancelled or the listener closes.
func (s *Server) Serve(ctx context.Context, ln net.Listener) error {
	s.mu.Lock()
	s.lns = append(s.lns, ln)
	s.mu.Unlock()

	for {
		conn, err := ln.Accept()
		if err != nil {
			s.mu.Lock()
			closing := s.close
			s.mu.Unlock()
			if closing || errors.Is(err, net.ErrClosed) || ctx.Err() != nil {
				return nil
			}
			return err
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handle(ctx, conn)
		}()
	}
}

// Shutdown stops accepting and waits for in-flight sessions.
func (s *Server) Shutdown() {
	s.mu.Lock()
	s.close = true
	lns := s.lns
	s.lns = nil
	s.mu.Unlock()
	for _, ln := range lns {
		_ = ln.Close()
	}
	s.wg.Wait()
}

type session struct {
	srv   *Server
	conn  net.Conn
	r     *bufio.Reader
	ip    string
	id    int64
	helo  bool
	mail  bool
	rcpts int
	tls   bool
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	ip := hostOf(conn.RemoteAddr())
	id := s.eng.NextConnID()
	admitted := false

	defer func() {
		s.eng.OnDisconnect(ip, admitted)
		_ = conn.Close()
	}()

	verdict, ok := s.eng.OnConnect(ip)
	admitted = ok

	if d := s.eng.TarpitBanner(); d > 0 {
		if !sleepCtx(ctx, d) {
			return
		}
	}

	ses := &session{srv: s, conn: conn, r: bufio.NewReaderSize(conn, maxLineBytes), ip: ip, id: id}

	if !ok {
		ses.write(verdict.Reply)
		return
	}
	ses.write(verdict.Reply)

	prof := s.eng.Profile()
	idle := prof.Behaviour.IdleTimeout.D()

	for {
		if ctx.Err() != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Now().Add(idle))
		line, err := ses.readLine()
		if err != nil {
			if !errors.Is(err, io.EOF) {
				s.log.Debug("session ended", "ip", ip, "conn", id, "err", err)
			}
			return
		}
		s.eng.Log(ip, id, "C", line)
		if cont := ses.dispatch(ctx, line); !cont {
			return
		}
	}
}

func (ses *session) readLine() (string, error) {
	line, err := ses.r.ReadString('\n')
	if err != nil {
		if len(line) > 0 && errors.Is(err, io.EOF) {
			return strings.TrimRight(line, "\r\n"), nil
		}
		return "", err
	}
	if len(line) >= maxLineBytes {
		return "", fmt.Errorf("line too long")
	}
	return strings.TrimRight(line, "\r\n"), nil
}

func (ses *session) write(s string) {
	if s == "" {
		return
	}
	_ = ses.conn.SetWriteDeadline(time.Now().Add(30 * time.Second))
	if _, err := io.WriteString(ses.conn, s+"\r\n"); err != nil {
		return
	}
	ses.srv.eng.Log(ses.ip, ses.id, "S", s)
}

// dispatch returns false when the connection should close.
func (ses *session) dispatch(ctx context.Context, line string) bool {
	cmd, arg := splitCmd(line)
	eng := ses.srv.eng
	prof := eng.Profile()

	switch cmd {
	case "EHLO", "HELO":
		if arg == "" {
			ses.write("501 5.5.4 Syntax: " + cmd + " hostname")
			return true
		}
		ses.helo, ses.mail, ses.rcpts = true, false, 0
		host := hostFromBanner(prof.Banner, prof.Name)
		if cmd == "HELO" {
			ses.write("250 " + host)
			return true
		}
		ses.write("250-" + host)
		caps := prof.EhloCaps
		if offersTLS(prof.StartTLS) && !ses.tls {
			caps = append(append([]string{}, caps...), "STARTTLS")
		}
		for i, c := range caps {
			if i == len(caps)-1 {
				ses.write("250 " + c)
			} else {
				ses.write("250-" + c)
			}
		}
		return true

	case "MAIL":
		if !ses.helo {
			eng.CountBadSequence()
			ses.write("503 5.5.1 Error: send HELO/EHLO first")
			return true
		}
		if !strings.HasPrefix(strings.ToUpper(arg), "FROM:") {
			ses.write("501 5.5.4 Syntax: MAIL FROM:<address>")
			return true
		}
		if prof.StartTLS == "required" && !ses.tls {
			ses.write("530 5.7.0 Must issue a STARTTLS command first")
			return true
		}
		ses.mail, ses.rcpts = true, 0
		ses.write("250 2.1.0 Ok")
		return true

	case "RCPT":
		if !ses.mail {
			// This is the case that proves a 503 is our client's bug and not
			// a provider block. Counted separately on purpose.
			eng.CountBadSequence()
			ses.write("503 5.5.1 Error: need MAIL command")
			return true
		}
		if !strings.HasPrefix(strings.ToUpper(arg), "TO:") {
			ses.write("501 5.5.4 Syntax: RCPT TO:<address>")
			return true
		}
		addr := parseAddr(arg[len("TO:"):])
		if addr == "" {
			ses.write("501 5.1.3 Bad recipient address syntax")
			return true
		}
		ses.rcpts++
		if d := eng.TarpitRcpt(); d > 0 {
			if !sleepCtx(ctx, d) {
				return false
			}
		}
		v := eng.OnRcpt(ses.ip, addr, ses.rcpts)
		switch v.Action {
		case policy.ActionDrop:
			ses.hardClose()
			return false
		case policy.ActionTimeout:
			hold := prof.Behaviour.TimeoutHold.D()
			sleepCtx(ctx, hold)
			return false
		case policy.ActionClose:
			ses.write(v.Reply)
			return false
		default:
			ses.write(v.Reply)
			return true
		}

	case "RSET":
		ses.mail, ses.rcpts = false, 0
		ses.write("250 2.0.0 Ok")
		return true

	case "NOOP":
		ses.write("250 2.0.0 Ok")
		return true

	case "VRFY":
		// Never confirm. Real MXes stopped answering VRFY decades ago, and a
		// validator that trusts it will be wrong in production.
		ses.write("252 2.5.2 Cannot VRFY user, but will accept message and attempt delivery")
		return true

	case "STARTTLS":
		if !offersTLS(prof.StartTLS) || ses.tls {
			ses.write("454 4.7.0 TLS not available")
			return true
		}
		ses.write("220 2.0.0 Ready to start TLS")
		if prof.StartTLS == "broken" {
			// Agree, then never hold up our end of the handshake.
			ses.hardClose()
			return false
		}
		cert, err := serverCert()
		if err != nil {
			return false
		}
		tc := tls.Server(ses.conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		_ = ses.conn.SetDeadline(time.Now().Add(10 * time.Second))
		if err := tc.HandshakeContext(ctx); err != nil {
			return false
		}
		_ = ses.conn.SetDeadline(time.Time{})
		// RFC 3207 §4.2: everything learned before TLS is forgotten.
		ses.conn, ses.r = tc, bufio.NewReaderSize(tc, maxLineBytes)
		ses.tls, ses.helo, ses.mail, ses.rcpts = true, false, false, 0
		return true

	case "DATA":
		if !ses.mail || ses.rcpts == 0 {
			eng.CountBadSequence()
			ses.write("503 5.5.1 Error: need RCPT command")
			return true
		}
		ses.write("354 End data with <CR><LF>.<CR><LF>")
		for {
			_ = ses.conn.SetReadDeadline(time.Now().Add(prof.Behaviour.IdleTimeout.D()))
			l, err := ses.readLine()
			if err != nil {
				return false
			}
			if l == "." {
				break
			}
		}
		ses.mail, ses.rcpts = false, 0
		ses.write("250 2.0.0 Ok: queued as SIMULATED")
		return true

	case "QUIT":
		ses.write("221 2.0.0 Bye")
		return false

	case "":
		ses.write("500 5.5.2 Error: bad syntax")
		return true

	default:
		ses.write("500 5.5.2 Error: command not recognized")
		return true
	}
}

// hardClose sends a RST rather than a FIN, which is what a real mid-session
// drop looks like to the client.
func (ses *session) hardClose() {
	if tc, ok := ses.conn.(*net.TCPConn); ok {
		_ = tc.SetLinger(0)
	}
	ses.srv.eng.Log(ses.ip, ses.id, "S", "<connection reset>")
	_ = ses.conn.Close()
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

func splitCmd(line string) (cmd, arg string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", ""
	}
	if i := strings.IndexAny(line, " \t"); i >= 0 {
		return strings.ToUpper(line[:i]), strings.TrimSpace(line[i+1:])
	}
	return strings.ToUpper(line), ""
}

// parseAddr pulls the address out of "<user@host>" or "user@host SIZE=1".
func parseAddr(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "<"); i >= 0 {
		if j := strings.Index(s[i:], ">"); j > 0 {
			return strings.TrimSpace(s[i+1 : i+j])
		}
		return ""
	}
	if i := strings.IndexAny(s, " \t"); i >= 0 {
		s = s[:i]
	}
	return s
}

func hostOf(a net.Addr) string {
	h, _, err := net.SplitHostPort(a.String())
	if err != nil {
		return a.String()
	}
	return h
}

func hostFromBanner(banner, fallback string) string {
	f := strings.Fields(banner)
	if len(f) >= 2 {
		return f[1]
	}
	return "mx." + fallback + ".test"
}

func offersTLS(mode string) bool {
	return mode == "offered" || mode == "required" || mode == "broken"
}

var (
	certOnce sync.Once
	certVal  tls.Certificate
	certErr  error
)

// serverCert is the simulator's one self-signed certificate, made on first use.
// It names nothing in particular on purpose: a real MX certificate rarely names
// the MX either, and the prober must complete the session regardless (plan 031).
func serverCert() (tls.Certificate, error) {
	certOnce.Do(func() {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			certErr = err
			return
		}
		tmpl := &x509.Certificate{
			SerialNumber: big.NewInt(1),
			Subject:      pkix.Name{CommonName: "mxsim.invalid"},
			DNSNames:     []string{"mxsim.invalid"},
			NotBefore:    time.Now().Add(-time.Hour),
			NotAfter:     time.Now().Add(24 * time.Hour),
			KeyUsage:     x509.KeyUsageDigitalSignature,
			ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
		if err != nil {
			certErr = err
			return
		}
		certVal = tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	})
	return certVal, certErr
}

// Certificate is the simulator's certificate, for a test that wants to trust
// it and see the prober report "verified".
func Certificate() (*x509.Certificate, error) {
	c, err := serverCert()
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(c.Certificate[0])
}
