// Package redis is a hand-rolled RESP client covering exactly the commands this
// service needs. Pulling in a full client library to run one Lua script and a
// handful of key reads would be the largest dependency in the repo.
//
// The wire encoding and reply decoding are ported from
// ../ds-smtp-retry/ratecheck/internal/redis. What is not ported is that client's
// connection handling: it dials a fresh TCP connection per command, which is
// fine for a calibration CLI and wrong here, where the token bucket is taken
// once per probe on the hot path.
package redis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// Error is a reply Redis itself rejected, as opposed to a transport failure.
// The two are different for invariant 5: a malformed script is our bug, an
// unreachable server means fail closed.
type Error struct{ Msg string }

func (e *Error) Error() string { return "redis: " + e.Msg }

// encode renders one command in the RESP array form Redis expects.
func encode(b *strings.Builder, args []string) {
	fmt.Fprintf(b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(b, "$%d\r\n%s\r\n", len(a), a)
	}
}

// readReply decodes one reply. Nil is returned for RESP null, which callers
// must distinguish from an empty string.
func readReply(r *bufio.Reader) (any, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, errors.New("redis: empty reply")
	}

	switch line[0] {
	case '+':
		return line[1:], nil
	case '-':
		return nil, &Error{Msg: line[1:]}
	case ':':
		return strconv.ParseInt(line[1:], 10, 64)
	case '$':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad bulk length %q: %w", line, err)
		}
		if n < 0 {
			return nil, nil
		}
		buf := make([]byte, n+2) // payload plus CRLF
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad array length %q: %w", line, err)
		}
		if n < 0 {
			return nil, nil
		}
		out := make([]any, 0, n)
		for range n {
			v, err := readReply(r)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}
