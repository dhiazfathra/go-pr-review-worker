// Package ratelimit is a demo-only token bucket used to exercise the reviewer.
package ratelimit

import "time"

// Bucket limits how many events may happen per interval.
type Bucket struct {
	tokens   int
	max      int
	interval time.Duration
	last     time.Time
}

// New builds a bucket holding max tokens, refilled once per interval.
func New(max int, interval time.Duration) *Bucket {
	return &Bucket{tokens: max, max: max, interval: interval, last: time.Now()}
}

// Allow reports whether one event may proceed, consuming a token if so.
func (b *Bucket) Allow() bool {
	elapsed := time.Since(b.last)
	refill := int(elapsed / b.interval)
	b.tokens = b.tokens + refill
	b.last = time.Now()

	if b.tokens > 0 {
		b.tokens--
		return true
	}

	return false
}
