package resolver

import (
	"context"
	"errors"
	"net"
	"testing"
)

// MX ordering and the two answers that are not failures.
func TestMXReturnsHostsInPreferenceOrder(t *testing.T) {
	r := New(Options{
		LookupMX: func(context.Context, string) ([]*net.MX, error) {
			return []*net.MX{
				{Host: "backup.example.com.", Pref: 30},
				{Host: "primary.example.com.", Pref: 10},
				{Host: "second.example.com.", Pref: 20},
			}, nil
		},
	})
	hosts, err := r.MX(t.Context(), "example.com")
	if err != nil {
		t.Fatalf("MX: %v", err)
	}
	want := []string{"primary.example.com", "second.example.com", "backup.example.com"}
	if len(hosts) != len(want) {
		t.Fatalf("MX = %v, want %v", hosts, want)
	}
	for i := range want {
		if hosts[i] != want[i] {
			t.Fatalf("MX = %v, want %v — the trailing dot must go and preference must sort", hosts, want)
		}
	}
}

// RFC 7505: a single "." is the domain saying it accepts no mail. Treating that
// as an empty answer would retry a message forever against a domain that has
// said, explicitly, not to.
func TestANullMXIsAnAnswerAndNotAnEmptyResult(t *testing.T) {
	r := New(Options{
		LookupMX: func(context.Context, string) ([]*net.MX, error) {
			return []*net.MX{{Host: ".", Pref: 0}}, nil
		},
	})
	_, err := r.MX(t.Context(), "example.com")
	if !errors.Is(err, ErrNullMX) {
		t.Fatalf("MX error = %v, want ErrNullMX", err)
	}
}

func TestNoMXRecordsIsItsOwnError(t *testing.T) {
	r := New(Options{
		LookupMX: func(context.Context, string) ([]*net.MX, error) { return nil, nil },
	})
	if _, err := r.MX(t.Context(), "example.com"); !errors.Is(err, ErrNoMX) {
		t.Fatalf("MX error = %v, want ErrNoMX", err)
	}
}
