package redis

import (
	"bufio"
	"strconv"
	"strings"
	"testing"
)

// Plan 027, fix 2. A hostile length used to reach make() unchecked:
// "*1125899906842624\r\n" panicked with "makeslice: cap out of range", and a
// panic on a request goroutine takes the process down. An over-limit length is
// an error now, and so is nesting past maxDepth.
func TestReadReplyRefusesHostileLengths(t *testing.T) {
	for name, wire := range map[string]string{
		"huge array":          "*1125899906842624\r\n",
		"huge bulk":           "$1125899906842624\r\n",
		"max int array":       "*9223372036854775807\r\n",
		"max int bulk":        "$9223372036854775807\r\n",
		"array over the cap":  "*1048577\r\n",
		"bulk over the cap":   "$536870913\r\n",
		"bulk short of bytes": "$10\r\nabc\r\n",
		"nesting past depth":  strings.Repeat("*1\r\n", maxDepth+1) + ":1\r\n",
		"bad array length":    "*x\r\n",
		"bad bulk length":     "$x\r\n",
		"bad integer":         ":x\r\n",
		"empty line":          "\r\n",
		"unknown type":        "!oops\r\n",
		"truncated":           "",
		"array member broken": "*2\r\n:1\r\n",
	} {
		t.Run(name, func(t *testing.T) {
			if v, err := readReply(bufio.NewReader(strings.NewReader(wire))); err == nil {
				t.Errorf("readReply(%.40q) = %#v, nil; want an error", wire, v)
			}
		})
	}
}

// Depth up to the bound still decodes: the bound exists to stop recursion, not
// to refuse the shapes this service actually reads.
func TestReadReplyAcceptsNestingAtDepth(t *testing.T) {
	wire := strings.Repeat("*1\r\n", maxDepth) + ":7\r\n"
	v, err := readReply(bufio.NewReader(strings.NewReader(wire)))
	if err != nil {
		t.Fatalf("readReply at depth %d: %v", maxDepth, err)
	}
	for range maxDepth {
		arr, ok := v.([]any)
		if !ok || len(arr) != 1 {
			t.Fatalf("got %#v, want a one-element array", v)
		}
		v = arr[0]
	}
	if v != int64(7) {
		t.Errorf("innermost = %#v, want 7", v)
	}
}

// A large bulk string that is really there is still read whole. The path that
// grows with the data (rather than trusting the length) must not truncate.
func TestReadReplyLargeBulk(t *testing.T) {
	payload := strings.Repeat("z", smallBulk*3+17)
	wire := "$" + strconv.Itoa(len(payload)) + "\r\n" + payload + "\r\n"
	v, err := readReply(bufio.NewReader(strings.NewReader(wire)))
	if err != nil || v != payload {
		t.Fatalf("readReply(large bulk) = %d bytes, %v; want %d bytes", len(asString(v)), err, len(payload))
	}
}

func asString(v any) string { s, _ := v.(string); return s }

func FuzzRESPReply(f *testing.F) {
	for _, s := range []string{
		"+OK\r\n", "-ERR x\r\n", ":42\r\n", "$5\r\nhello\r\n", "$-1\r\n", "*-1\r\n",
		"*2\r\n:1\r\n$3\r\nabc\r\n", "*1125899906842624\r\n", "$1125899906842624\r\n",
		strings.Repeat("*1\r\n", 40), "*1\r\n*2\r\n:1\r\n:2\r\n", "",
	} {
		f.Add(s)
	}
	f.Fuzz(func(_ *testing.T, wire string) {
		// The property: no panic, whatever arrives. readReply either decodes
		// or returns an error; allocation is bounded by the input, which the
		// fuzzer's own memory limit enforces.
		_, _ = readReply(bufio.NewReader(strings.NewReader(wire)))
	})
}

// Encode→decode round-trips for any argument list: the fake server in
// redis_test.go reads commands with readReply, so the two must agree.
func FuzzRESPRoundTrip(f *testing.F) {
	f.Add("GET", "key", "")
	f.Add("EVALSHA", "abc\r\ndef", "$5\r\n")
	f.Fuzz(func(t *testing.T, a, b, c string) {
		var sb strings.Builder
		encode(&sb, []string{a, b, c})
		v, err := readReply(bufio.NewReader(strings.NewReader(sb.String())))
		if err != nil {
			t.Fatalf("decode(encode(%q,%q,%q)): %v", a, b, c, err)
		}
		if !equal(v, []any{a, b, c}) {
			t.Fatalf("round trip = %#v, want %q", v, []string{a, b, c})
		}
	})
}
