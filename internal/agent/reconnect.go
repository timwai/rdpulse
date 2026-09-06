package agent

import (
	"math/rand"
	"time"
)

// Backoff manages exponential backoff delay with jitter
type Backoff struct {
	initialInterval time.Duration
	maxInterval     time.Duration
	currentInterval time.Duration
	multiplier      float64
	jitterRatio     float64
}

// NewBackoff creates a Backoff instance
func NewBackoff(initial, max time.Duration) *Backoff {
	if initial <= 0 {
		initial = 1 * time.Second
	}
	if max <= 0 {
		max = 30 * time.Second
	}
	return &Backoff{
		initialInterval: initial,
		maxInterval:     max,
		currentInterval: initial,
		multiplier:      2.0,
		jitterRatio:     0.2, // +/- 20%
	}
}

// Reset resets backoff to initial state
func (b *Backoff) Reset() {
	b.currentInterval = b.initialInterval
}

// NextDelay calculates the next delay with jitter applied
func (b *Backoff) NextDelay() time.Duration {
	base := float64(b.currentInterval)

	// Jitter between -20% and +20%
	jitterRange := base * b.jitterRatio * 2
	jitter := (rand.Float64() * jitterRange) - (base * b.jitterRatio)
	delay := time.Duration(base + jitter)

	// Advance current interval exponentially up to maxInterval
	next := time.Duration(float64(b.currentInterval) * b.multiplier)
	if next > b.maxInterval {
		b.currentInterval = b.maxInterval
	} else {
		b.currentInterval = next
	}

	return delay
}
