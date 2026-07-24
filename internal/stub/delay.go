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
	delay := &Delay{Min: min, Max: max}
	if err := delay.validate(); err != nil {
		return nil, err
	}

	return delay, nil
}

func (d *Delay) validate() error {
	if d == nil {
		return fmt.Errorf("delay must not be nil")
	}
	if d.Min < 0 {
		return fmt.Errorf("delay minimum must not be negative: %s", d.Min)
	}
	if d.Max < d.Min {
		return fmt.Errorf("delay maximum %s must not be less than minimum %s", d.Max, d.Min)
	}
	return nil
}

func (d *Delay) pick() time.Duration {
	if err := d.validate(); err != nil {
		return 0
	}
	if d.Min == d.Max {
		return d.Min
	}
	width := uint64(d.Max-d.Min) + 1
	return d.Min + time.Duration(rand.N(width))
}

// Wait blocks for the selected delay or until the context is canceled.
func (d *Delay) Wait(ctx context.Context) error {
	if d == nil {
		return ctx.Err()
	}
	if err := d.validate(); err != nil {
		return err
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
