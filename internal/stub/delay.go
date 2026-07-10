package stub

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"time"
)

// Delay is a fixed or ranged response delay. Min and Max are inclusive.
type Delay struct {
	Min time.Duration
	Max time.Duration
}

// ParseDelay parses a fixed duration or an inclusive min..max range.
func ParseDelay(value string) (*Delay, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil, nil
	}

	minText, maxText, ranged := strings.Cut(value, "..")
	min, err := time.ParseDuration(strings.TrimSpace(minText))
	if err != nil {
		return nil, fmt.Errorf("parsing delay minimum %q: %w", strings.TrimSpace(minText), err)
	}
	max := min
	if ranged {
		max, err = time.ParseDuration(strings.TrimSpace(maxText))
		if err != nil {
			return nil, fmt.Errorf("parsing delay maximum %q: %w", strings.TrimSpace(maxText), err)
		}
	}
	if min < 0 {
		return nil, fmt.Errorf("delay minimum must not be negative: %s", min)
	}
	if max < min {
		return nil, fmt.Errorf("delay maximum %s must not be less than minimum %s", max, min)
	}

	return &Delay{Min: min, Max: max}, nil
}

func (d *Delay) pick() time.Duration {
	if d.Min == d.Max {
		return d.Min
	}
	span := d.Max - d.Min
	return d.Min + rand.N(span+1)
}

// Wait blocks for the selected delay or until the context is canceled.
func (d *Delay) Wait(ctx context.Context) error {
	if d == nil {
		return ctx.Err()
	}
	duration := d.pick()
	if duration <= 0 {
		return ctx.Err()
	}

	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
