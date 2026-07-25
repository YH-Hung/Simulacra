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
	cancel context.CancelFunc
	done   <-chan struct{}
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
	watcher, started := startReadyWatcher(parent, func(ctx context.Context, ready func()) error {
		return watch(ctx, dirs, stub.WatchOptions{
			Debounce: 200 * time.Millisecond,
			OnChange: onChange,
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

func startReadyWatcher(parent context.Context, run func(context.Context, func()) error, report func(error)) (watcherRun, <-chan error) {
	ctx, cancel := context.WithCancel(parent)
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

func (w watcherRun) stop() {
	w.cancelNow()
	if w.done != nil {
		<-w.done
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
	store.Replace(stubs)
	if announce && ctx.Err() == nil {
		reporter.Printf("simulacra: %d stub(s) reloaded\n", len(stubs))
	}
	return len(stubs), nil
}
