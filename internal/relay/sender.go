package relay

import (
	"bufio"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Outcome is what one delivery attempt concluded.
type Outcome struct {
	Delivered bool
	// Permanent marks a 5xx: the message will never be accepted, and retrying
	// spends the IP's standing on a question already answered.
	Permanent bool
	Code      int
	Reply     string
	Err       string
	// TLS records whether the transfer was encrypted. Kept because "was this
	// message sent in the clear" is asked after the fact and cannot be
	// reconstructed later.
	TLS bool
}

// SendOptions is one delivery. Everything here is decided before the socket
// opens — the queue owns retries, the pacer owns timing.
type SendOptions struct {
	MXHost     string
	Addr       net.Addr
	Helo       string
	MailFrom   string
	RcptTo     string
	Data       []byte
	Timeout    time.Duration
	DialTLS    bool
	SkipVerify bool
}

// Send performs one SMTP delivery over an already-dialled connection.
//
// The connection is dialled by the caller so the SSRF guard and the resolver
// stay in one place (invariant 2): an MX host is attacker-influenced here just
// as it is for verification, because the recipient domain chooses it.
func Send(ctx context.Context, conn net.Conn, opt SendOptions) Outcome {
	defer func() { _ = conn.Close() }()

	timeout := opt.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	} else {
		_ = conn.SetDeadline(time.Now().Add(timeout))
	}

	r := bufio.NewReader(conn)
	var netErr error
	step := func(cmd string) (int, string, bool) {
		if cmd != "" {
			if _, err := io.WriteString(conn, cmd+"\r\n"); err != nil {
				netErr = err
				return 0, "", false
			}
		}
		code, text, err := readFullReply(r)
		if err != nil {
			netErr = err
			return 0, "", false
		}
		return code, text, true
	}
	transport := func() Outcome {
		return Outcome{Err: netErr.Error()}
	}

	code, text, ok := step("")
	if !ok {
		return transport()
	}
	if code != 220 {
		return reply(code, text)
	}

	code, text, ok = step("EHLO " + opt.Helo)
	if !ok {
		return transport()
	}
	if code != 250 {
		return reply(code, text)
	}

	usedTLS := false
	if strings.Contains(strings.ToUpper(text), "STARTTLS") {
		if code, text, ok = step("STARTTLS"); !ok {
			return transport()
		}
		if code != 220 {
			return reply(code, text)
		}
		// Opportunistic, and deliberately not verified. Almost no MX presents a
		// certificate matching the name we looked up, so requiring validity
		// would send everything in the clear instead — which is strictly worse.
		// This buys confidentiality against a passive observer, not proof of
		// who is listening.
		tlsConn := tls.Client(conn, &tls.Config{
			ServerName:         opt.MXHost,
			InsecureSkipVerify: true, //nolint:gosec // see above
			MinVersion:         tls.VersionTLS12,
		})
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			netErr = err
			return transport()
		}
		conn = tlsConn
		usedTLS = true
		r = bufio.NewReader(conn)
		// EHLO again: the session restarts after STARTTLS and anything the
		// server said before it must be discarded (RFC 3207 §4.2).
		if code, text, ok = step("EHLO " + opt.Helo); !ok {
			return transport()
		}
		if code != 250 {
			return reply(code, text)
		}
	}

	if code, text, ok = step("MAIL FROM:<" + opt.MailFrom + ">"); !ok {
		return transport()
	}
	if code != 250 {
		return reply(code, text)
	}
	if code, text, ok = step("RCPT TO:<" + opt.RcptTo + ">"); !ok {
		return transport()
	}
	if code != 250 && code != 251 {
		return withTLS(reply(code, text), usedTLS)
	}
	if code, text, ok = step("DATA"); !ok {
		return transport()
	}
	if code != 354 {
		return withTLS(reply(code, text), usedTLS)
	}

	// dotStuff appends the terminating ".\r\n" itself, so the reply is *read*
	// here and not prompted for. Sending another dot would be a second
	// terminator — a stray command after the message a real server answers 500
	// to, and on a synchronous connection a deadlock: both ends writing, with
	// nobody reading.
	if _, err := conn.Write(dotStuff(opt.Data)); err != nil {
		netErr = err
		return transport()
	}
	if code, text, ok = step(""); !ok {
		return transport()
	}
	_, _, _ = step("QUIT")

	if code == 250 {
		return Outcome{Delivered: true, Code: code, Reply: text, TLS: usedTLS}
	}
	return withTLS(reply(code, text), usedTLS)
}

func reply(code int, text string) Outcome {
	return Outcome{Permanent: code >= 500, Code: code, Reply: text}
}

func withTLS(o Outcome, used bool) Outcome { o.TLS = used; return o }

// dotStuff applies transparency (RFC 5321 §4.5.2) and terminates the data.
//
// A line that is a single dot ends the message. A body line that happens to
// start with one must be doubled, or the message is truncated there and
// whatever follows is read as SMTP commands — which is how a body becomes an
// injection.
func dotStuff(data []byte) []byte {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, ".") {
			lines[i] = "." + line
		}
	}
	out := strings.Join(lines, "\r\n")
	if !strings.HasSuffix(out, "\r\n") {
		out += "\r\n"
	}
	return []byte(out + ".\r\n")
}

// readFullReply returns the code and **every line** of a multi-line reply.
//
// The prober keeps only the last line, which is all a verdict needs. Sending
// needs the whole thing: STARTTLS is announced on its own continuation line of
// the EHLO response, and reading only the last would silently send every
// message in the clear.
func readFullReply(r *bufio.Reader) (int, string, error) {
	var all []string
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return 0, "", err
		}
		line = strings.TrimRight(line, "\r\n")
		if len(line) < 3 {
			return 0, line, fmt.Errorf("relay: short reply %q", line)
		}
		code, err := strconv.Atoi(line[:3])
		if err != nil {
			return 0, line, fmt.Errorf("relay: bad reply %q", line)
		}
		all = append(all, line)
		if len(line) > 3 && line[3] == '-' {
			continue
		}
		return code, strings.Join(all, "\n"), nil
	}
}
