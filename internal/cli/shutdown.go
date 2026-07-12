package cli

import (
	"context"
	"os"
	"time"
)

// waitAndShutdown blocks until a signal arrives, then attempts a graceful
// stop. Because graceful stop waits for in-flight RPCs (including open
// streams) it can block forever; a second signal or the timeout falls back
// to force, which aborts remaining connections so the process can exit.
func waitAndShutdown(sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	waitAndShutdownContext(context.Background(), sig, timeout, graceful, force, logln)
}

func waitAndShutdownContext(ctx context.Context, sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	select {
	case <-ctx.Done():
		return
	case <-sig:
	}
	logln("simulacra: shutting down (interrupt again to force)")
	done := make(chan struct{})
	go func() {
		graceful()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-done:
	case <-sig:
		logln("simulacra: forcing stop")
		force()
	case <-timer.C:
		logln("simulacra: graceful shutdown timed out; forcing stop")
		force()
	}
}
