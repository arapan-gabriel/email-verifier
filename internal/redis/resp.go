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
	"bytes"
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

// Bounds on what a reply may claim. Redis is local and trusted, but a length
// read off the wire used to reach make() unchecked: "*1125899906842624"
// panicked with "makeslice: cap out of range", and a panic on a request
// goroutine takes the whole process down (plan 027). An over-limit value is an
// error, which callers already treat as a transport failure — fail closed.
const (
	// maxBulkLen is Redis's own proto-max-bulk-len default (512 MiB).
	maxBulkLen = 512 << 20
	// maxArrayLen is far beyond any reply this service reads (a relay
	// ZRANGEBYSCORE page is the largest) and small enough that a claim of it
	// cannot exhaust memory before the elements arrive.
	maxArrayLen = 1 << 20
	// maxDepth bounds recursion. Nothing here reads deeper than two.
	maxDepth = 8
	// smallBulk is the largest bulk string allocated up front. Above it the
	// buffer grows with the bytes actually received, so a length that lies
	// costs nothing until the data is really there.
	smallBulk = 64 << 10
	// arrayPrealloc caps the capacity reserved from a claimed array length.
	arrayPrealloc = 64
)

// readReply decodes one reply. Nil is returned for RESP null, which callers
// must distinguish from an empty string.
func readReply(r *bufio.Reader) (any, error) {
	return readValue(r, 0)
}

func readValue(r *bufio.Reader, depth int) (any, error) {
	if depth > maxDepth {
		return nil, fmt.Errorf("redis: reply nested deeper than %d", maxDepth)
	}
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
		if n > maxBulkLen {
			return nil, fmt.Errorf("redis: bulk length %d over the %d limit", n, maxBulkLen)
		}
		return readBulk(r, n)
	case '*':
		n, err := strconv.Atoi(line[1:])
		if err != nil {
			return nil, fmt.Errorf("redis: bad array length %q: %w", line, err)
		}
		if n < 0 {
			return nil, nil
		}
		if n > maxArrayLen {
			return nil, fmt.Errorf("redis: array length %d over the %d limit", n, maxArrayLen)
		}
		out := make([]any, 0, min(n, arrayPrealloc))
		for range n {
			v, err := readValue(r, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, fmt.Errorf("redis: unexpected reply %q", line)
}

// readBulk reads n payload bytes and the CRLF after them.
func readBulk(r *bufio.Reader, n int) (string, error) {
	if n <= smallBulk {
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return "", err
		}
		return string(buf[:n]), nil
	}
	var b bytes.Buffer
	b.Grow(smallBulk)
	if _, err := io.CopyN(&b, r, int64(n)+2); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	return string(b.Bytes()[:n]), nil
}
