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
// Context cancellation initiates the same sequence, but forces promptly
// because a canceled command is no longer granting a graceful wait period.
func waitAndShutdown(sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	waitAndShutdownContext(context.Background(), sig, timeout, graceful, force, logln)
}

func waitAndShutdownContext(ctx context.Context, sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	waitAndShutdownContextStop(ctx, nil, sig, timeout, graceful, force, logln)
}

// stopWaiter only dismisses a waiter that has not begun shutdown. Once a
// signal or command cancellation starts graceful shutdown, only graceful
// completion, command cancellation, another signal, or the timeout ends it.
func waitAndShutdownContextStop(ctx context.Context, stopWaiter <-chan struct{}, sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	commandCanceled := false
	select {
	case <-ctx.Done():
		commandCanceled = true
	case <-stopWaiter:
		return
	case <-sig:
	}
	if commandCanceled {
		logln("simulacra: command canceled; shutting down")
	} else {
		logln("simulacra: shutting down (interrupt again to force)")
	}
	done := make(chan struct{})
	go func() {
		graceful()
		close(done)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	forceAndJoin := func() {
		force()
		<-done
	}
	select {
	case <-ctx.Done():
		logln("simulacra: command canceled; forcing stop")
		forceAndJoin()
	case <-done:
	case <-sig:
		logln("simulacra: forcing stop")
		forceAndJoin()
	case <-timer.C:
		logln("simulacra: graceful shutdown timed out; forcing stop")
		forceAndJoin()
	}
}
