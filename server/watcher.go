package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

type watcherRun struct {
	cancel  context.CancelFunc
	done    <-chan struct{}
	reloads *reloadTracker
}

// reloadTracker counts in-flight OnChange calls. stub.WatchWithOptions runs
// OnChange on a worker goroutine it does not join — cancellation stops event
// collection immediately, by design, even mid-callback — so joining the watch
// goroutine alone would let a reload keep swapping the store and writing to the
// Reporter after Shutdown returned.
type reloadTracker struct {
	mu     sync.Mutex
	idle   sync.Cond
	active int
	closed bool
}

func newReloadTracker() *reloadTracker {
	t := &reloadTracker{}
	t.idle.L = &t.mu
	return t
}

// track wraps onChange so the call is visible to closeAndJoin for its whole
// duration, and so a call that enters after the tracker closed is dropped
// rather than run. The worker tests its context and calls OnChange as two
// separate steps, so cancellation alone cannot stop a call already between
// them; the closed flag can, because entry and closing take the same lock.
func (t *reloadTracker) track(onChange func(context.Context)) func(context.Context) {
	return func(ctx context.Context) {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		t.active++
		t.mu.Unlock()
		defer func() {
			t.mu.Lock()
			t.active--
			if t.active == 0 {
				t.idle.Broadcast()
			}
			t.mu.Unlock()
		}()
		onChange(ctx)
	}
}

// closeAndJoin refuses further calls and blocks until the ones already running
// have returned. Closing before waiting is what makes the join final: any call
// that has not taken the lock yet will find the tracker closed and do nothing.
func (t *reloadTracker) closeAndJoin() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	for t.active > 0 {
		t.idle.Wait()
	}
}

type watchDirsFunc func(context.Context, []string, stub.WatchOptions) error

// startStubWatcher does not return success until every initial watch is
// attached. Once attached, it reconciles the store while filesystem events
// are already being collected, closing the load-before-watch update window.
func startStubWatcher(parent context.Context, dirs []string, reporter Reporter, onChange func(context.Context), reconcile func(context.Context) error, watch watchDirsFunc) (watcherRun, error) {
	startupComplete := make(chan struct{})
	var completeStartup sync.Once
	finishStartup := func() {
		completeStartup.Do(func() { close(startupComplete) })
	}
	reloads := newReloadTracker()
	watcher, started := startReadyWatcher(parent, func(ctx context.Context, ready func()) error {
		return watch(ctx, dirs, stub.WatchOptions{
			Debounce: 200 * time.Millisecond,
			OnChange: reloads.track(onChange),
			OnError: func(err error) {
				reporter.PrintErrln("watch error:", err)
			},
			Ready: func() {
				ready()
				<-startupComplete
			},
		})
	}, func(err error) {
		reporter.PrintErrln("watch error:", err)
	})
	watcher.reloads = reloads

	select {
	case err := <-started:
		if err != nil {
			finishStartup()
			watcher.stop()
			return watcherRun{}, fmt.Errorf("starting stub watcher: %w", err)
		}
	case <-parent.Done():
		finishStartup()
		watcher.stop()
		return watcherRun{}, parent.Err()
	}
	if err := reconcile(parent); err != nil {
		watcher.cancelNow()
		finishStartup()
		watcher.stop()
		if parentErr := parent.Err(); parentErr != nil {
			return watcherRun{}, parentErr
		}
		return watcherRun{}, fmt.Errorf("reconciling stub watcher: %w", err)
	}
	finishStartup()
	if err := parent.Err(); err != nil {
		watcher.stop()
		return watcherRun{}, err
	}
	return watcher, nil
}

// startReadyWatcher detaches the watcher's lifetime from parent: parent scopes
// startup (the caller aborts by stopping the returned watcher), but once the
// watcher is running only the server's own cancel stops it. Deriving the run
// context from parent instead would let a caller's startup timeout silently
// disarm hot reload under a still-serving data plane.
func startReadyWatcher(parent context.Context, run func(context.Context, func()) error, report func(error)) (watcherRun, <-chan error) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(parent))
	done := make(chan struct{})
	started := make(chan error, 1)
	var startup sync.Once
	ready := func() {
		startup.Do(func() { started <- nil })
	}
	go func() {
		defer close(done)
		err := run(ctx, ready)
		if err == nil && ctx.Err() != nil {
			err = ctx.Err()
		}
		sentStartup := false
		startup.Do(func() {
			sentStartup = true
			started <- err
		})
		if !sentStartup && err != nil && !errors.Is(err, context.Canceled) {
			report(err)
		}
	}()
	return watcherRun{cancel: cancel, done: done}, started
}

func (w watcherRun) cancelNow() {
	if w.cancel != nil {
		w.cancel()
	}
}

// stop cancels the watcher, joins its goroutine, then closes out any reload the
// watcher had already started: in-flight calls are waited for, and a call still
// on its way in is dropped. It blocks for as long as an in-flight reload takes,
// which includes the Reporter calls it makes (see the Reporter contract). Once
// stop returns, no reload will touch the store or the Reporter again.
func (w watcherRun) stop() {
	w.cancelNow()
	if w.done != nil {
		<-w.done
	}
	if w.reloads != nil {
		w.reloads.closeAndJoin()
	}
}

// reconcileStubDirs reloads and recompiles every stub under dirs and atomically
// swaps them into the store. On any load error the store is left untouched and
// the errors are reported. Returns the number of stubs installed.
func reconcileStubDirs(ctx context.Context, reporter Reporter, reg *schema.Registry, store *stub.Store, dirs []string, announce bool) (int, error) {
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	stubs, errs := stub.LoadDirs(reg, dirs)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if len(errs) > 0 {
		for _, err := range errs {
			if ctx.Err() != nil {
				return 0, ctx.Err()
			}
			reporter.PrintErrln("stub error:", err)
		}
		return 0, errors.Join(errs...)
	}
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if _, err := store.ReplaceOrigin(stub.OriginFile, stubs); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		reporter.PrintErrln("stub error:", err)
		return 0, err
	}
	if announce && ctx.Err() == nil {
		reporter.Printf("simulacra: %d stub(s) reloaded\n", len(stubs))
	}
	return len(stubs), nil
}
