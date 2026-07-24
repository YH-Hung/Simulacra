package stub

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestParseDelay(t *testing.T) {
	tests := []struct {
		name string
		text string
		want *Delay
	}{
		{name: "empty", text: "", want: nil},
		{name: "fixed", text: "50ms", want: &Delay{Min: 50 * time.Millisecond, Max: 50 * time.Millisecond}},
		{name: "range", text: "50ms..200ms", want: &Delay{Min: 50 * time.Millisecond, Max: 200 * time.Millisecond}},
		{name: "range with spaces", text: "1s .. 2s", want: &Delay{Min: time.Second, Max: 2 * time.Second}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseDelay(tt.text)
			if err != nil {
				t.Fatalf("ParseDelay(%q) error = %v", tt.text, err)
			}
			if tt.want == nil {
				if got != nil {
					t.Fatalf("ParseDelay(%q) = %#v, want nil", tt.text, got)
				}
				return
			}
			if got == nil || *got != *tt.want {
				t.Fatalf("ParseDelay(%q) = %#v, want %#v", tt.text, got, tt.want)
			}
		})
	}
}

func TestParseDelayErrors(t *testing.T) {
	for _, text := range []string{
		"nonsense",
		"50ms..bad",
		"-1ms",
		"200ms..50ms",
	} {
		t.Run(text, func(t *testing.T) {
			if got, err := ParseDelay(text); err == nil {
				t.Fatalf("ParseDelay(%q) = %#v, want error", text, got)
			}
		})
	}
}

func TestDelayWaitCancelsPromptly(t *testing.T) {
	delay := &Delay{Min: 5 * time.Second, Max: 5 * time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	start := time.Now()
	err := delay.Wait(ctx)
	elapsed := time.Since(start)
	if err != context.DeadlineExceeded {
		t.Fatalf("Wait() error = %v, want %v", err, context.DeadlineExceeded)
	}
	if elapsed >= time.Second {
		t.Fatalf("Wait() took %v after context deadline, want prompt cancellation", elapsed)
	}
}

func TestDelayWaitNilAndZero(t *testing.T) {
	ctx := context.Background()
	var nilDelay *Delay
	if err := nilDelay.Wait(ctx); err != nil {
		t.Fatalf("nil Delay.Wait() error = %v, want nil", err)
	}
	if err := (&Delay{}).Wait(ctx); err != nil {
		t.Fatalf("zero Delay.Wait() error = %v, want nil", err)
	}
}

func TestDelayPickWithinInclusiveRange(t *testing.T) {
	delay := &Delay{Min: time.Millisecond, Max: 30 * time.Millisecond}
	for i := 0; i < 10; i++ {
		got := delay.pick()
		if got < delay.Min || got > delay.Max {
			t.Fatalf("pick() = %v, want within [%v, %v]", got, delay.Min, delay.Max)
		}
	}
}

func TestDelayPickIncludesBothEndpoints(t *testing.T) {
	delay := &Delay{Min: time.Second, Max: time.Second + time.Nanosecond}
	seen := map[time.Duration]bool{}
	for i := 0; i < 1_000; i++ {
		got := delay.pick()
		if got < delay.Min || got > delay.Max {
			t.Fatalf("pick() = %v, want within [%v, %v]", got, delay.Min, delay.Max)
		}
		seen[got] = true
	}
	if !seen[delay.Min] || !seen[delay.Max] {
		t.Fatalf("pick() endpoints seen = %v, want both %v and %v", seen, delay.Min, delay.Max)
	}
}

func TestDelayPickMaximumDurationRange(t *testing.T) {
	delay, err := ParseDelay("0s..2562047h47m16.854775807s")
	if err != nil {
		t.Fatalf("ParseDelay() error = %v", err)
	}
	for i := 0; i < 10; i++ {
		got := delay.pick()
		if got < delay.Min || got > delay.Max {
			t.Fatalf("pick() = %v, want within [%v, %v]", got, delay.Min, delay.Max)
		}
	}
}

func TestInvalidDelayIsSafe(t *testing.T) {
	tests := []struct {
		name  string
		delay *Delay
	}{
		{name: "negative minimum", delay: &Delay{Min: -time.Nanosecond, Max: time.Nanosecond}},
		{name: "reversed range", delay: &Delay{Min: 2 * time.Nanosecond, Max: time.Nanosecond}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.delay.pick(); got != 0 {
				t.Fatalf("invalid Delay.pick() = %v, want zero", got)
			}
			if err := tt.delay.Wait(context.Background()); err == nil {
				t.Fatal("invalid Delay.Wait() error = nil, want validation error")
			}
		})
	}
}

func TestDelayWaitAlreadyCanceled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	delay := &Delay{Min: 5 * time.Second, Max: 5 * time.Second}
	if err := delay.Wait(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Wait() error = %v, want %v", err, context.Canceled)
	}
}

func TestDelayWaitCompletes(t *testing.T) {
	delay := &Delay{Min: time.Nanosecond, Max: time.Nanosecond}
	if err := delay.Wait(context.Background()); err != nil {
		t.Fatalf("Wait() error = %v, want nil", err)
	}
}
