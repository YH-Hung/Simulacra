package cli

import (
	"os"
	"time"
)

// waitAndShutdown blocks until a signal arrives, then attempts a graceful
// stop. Because graceful stop waits for in-flight RPCs (including open
// streams) it can block forever; a second signal or the timeout falls back
// to force, which aborts remaining connections so the process can exit.
func waitAndShutdown(sig <-chan os.Signal, timeout time.Duration, graceful, force func(), logln func(a ...any)) {
	<-sig
	logln("simulacra: shutting down (interrupt again to force)")
	done := make(chan struct{})
	go func() {
		graceful()
		close(done)
	}()
	select {
	case <-done:
	case <-sig:
		logln("simulacra: forcing stop")
		force()
	case <-time.After(timeout):
		logln("simulacra: graceful shutdown timed out; forcing stop")
		force()
	}
}
