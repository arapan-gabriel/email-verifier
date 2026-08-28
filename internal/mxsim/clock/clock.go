// Package clock provides an injectable clock so time-dependent policy (rate
// windows, cooldowns, greylist expiry) can be fast-forwarded in tests instead
// of slept through.
package clock

import (
	"sync/atomic"
	"time"
)

// Clock is wall time that can be shifted forward.
type Clock interface {
	Now() time.Time
	Advance(d time.Duration) time.Duration
	Offset() time.Duration
}

// Offsetting is real time plus an offset that only ever moves forward.
type Offsetting struct {
	off atomic.Int64
}

func New() *Offsetting { return &Offsetting{} }

func (c *Offsetting) Now() time.Time {
	return time.Now().Add(time.Duration(c.off.Load()))
}

// Advance shifts the clock forward and returns the new total offset.
func (c *Offsetting) Advance(d time.Duration) time.Duration {
	if d < 0 {
		d = 0
	}
	return time.Duration(c.off.Add(int64(d)))
}

func (c *Offsetting) Offset() time.Duration { return time.Duration(c.off.Load()) }
