package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func newServeCmd() *cobra.Command {
	return newServeCmdWithListen(net.Listen)
}

func newServeCmdWithListen(serveListen func(string, string) (net.Listener, error)) *cobra.Command {
	src := &sources{}
	var listen string
	var journalSize int
	var watchStubs bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mock gRPC server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if journalSize <= 0 {
				return fmt.Errorf("journal-size must be greater than zero (got %d)", journalSize)
			}
			reg, err := src.buildRegistry(cmd.Context())
			if err != nil {
				return err
			}
			stubs, err := src.loadStubs(cmd, reg)
			if err != nil {
				return err
			}
			store := stub.NewStore(stubs)
			srv, err := dataplane.New(reg, store, journal.New(journalSize))
			if err != nil {
				return err
			}
			output := &commandOutput{cmd: cmd}
			watcher := watcherRun{}
			loadedStubCount := len(stubs)
			if watchStubs && len(src.stubDirs) > 0 {
				onChange := func(ctx context.Context) {
					reconcileStubDirsContext(ctx, output, reg, store, src.stubDirs, true)
				}
				watcher, err = startStubWatcher(cmd.Context(), src.stubDirs, output, onChange, func(ctx context.Context) {
					if count, replaced := reconcileStubDirsContext(ctx, output, reg, store, src.stubDirs, false); replaced {
						loadedStubCount = count
					}
				}, stub.WatchWithOptions)
				if err != nil {
					return err
				}
			}
			lis, err := serveListen("tcp", listen)
			if err != nil {
				watcher.stop()
				return fmt.Errorf("listening on %s: %w", listen, err)
			}

			output.Printf("simulacra: data plane listening on %s\n", lis.Addr())
			output.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				len(reg.Services()), loadedStubCount)

			sig := make(chan os.Signal, 2)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			stopShutdownWaiter := make(chan struct{})
			var stopShutdownWaiterOnce sync.Once
			cancelShutdown := func() {
				stopShutdownWaiterOnce.Do(func() { close(stopShutdownWaiter) })
			}
			shutdownDone := make(chan struct{})
			go func() {
				defer close(shutdownDone)
				waitAndShutdownContextStop(cmd.Context(), stopShutdownWaiter, sig, 10*time.Second, func() {
					watcher.cancelNow()
					srv.GracefulStop()
				}, func() {
					watcher.cancelNow()
					srv.Stop()
				}, output.Println)
			}()
			return serveWithRuntime(watcher, cancelShutdown, shutdownDone, func() error {
				return srv.Serve(lis)
			})
		},
	}
	src.register(cmd)
	cmd.Flags().StringVar(&listen, "listen", ":6565", "data-plane listen address")
	cmd.Flags().IntVar(&journalSize, "journal-size", 1024, "number of recent data-plane calls to retain")
	cmd.Flags().BoolVar(&watchStubs, "watch", true, "watch stub directories and reload changes")
	return cmd
}

type watcherRun struct {
	cancel context.CancelFunc
	done   <-chan struct{}
}

type watchDirsFunc func(context.Context, []string, stub.WatchOptions) error

// startStubWatcher does not return success until every initial watch is
// attached. Once attached, it reconciles the store while filesystem events
// are already being collected, closing the load-before-watch update window.
func startStubWatcher(parent context.Context, dirs []string, output reloadOutput, onChange, reconcile func(context.Context), watch watchDirsFunc) (watcherRun, error) {
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
				output.PrintErrln("watch error:", err)
			},
			Ready: func() {
				ready()
				<-startupComplete
			},
		})
	}, func(err error) {
		output.PrintErrln("watch error:", err)
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
	reconcile(parent)
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

func startWatcher(parent context.Context, run func(context.Context) error, report func(error)) watcherRun {
	watcher, _ := startReadyWatcher(parent, func(ctx context.Context, ready func()) error {
		ready()
		return run(ctx)
	}, report)
	return watcher
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

func serveWithRuntime(watcher watcherRun, cancelShutdown context.CancelFunc, shutdownDone <-chan struct{}, serve func() error) error {
	defer watcher.stop()
	defer func() {
		cancelShutdown()
		<-shutdownDone
	}()
	return serve()
}

type commandOutput struct {
	mu  sync.Mutex
	cmd *cobra.Command
}

func (o *commandOutput) Printf(format string, args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cmd.Printf(format, args...)
}

func (o *commandOutput) Println(args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cmd.Println(args...)
}

func (o *commandOutput) PrintErrln(args ...any) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.cmd.PrintErrln(args...)
}

type reloadOutput interface {
	Printf(string, ...any)
	PrintErrln(...any)
}

func reloadStubDirs(cmd *cobra.Command, reg *schema.Registry, store *stub.Store, dirs []string) {
	reloadStubDirsContext(context.Background(), &commandOutput{cmd: cmd}, reg, store, dirs)
}

func reloadStubDirsContext(ctx context.Context, output reloadOutput, reg *schema.Registry, store *stub.Store, dirs []string) {
	reconcileStubDirsContext(ctx, output, reg, store, dirs, true)
}

func reconcileStubDirsContext(ctx context.Context, output reloadOutput, reg *schema.Registry, store *stub.Store, dirs []string, announce bool) (int, bool) {
	if ctx.Err() != nil {
		return 0, false
	}
	stubs, errs := stub.LoadDirs(reg, dirs)
	if ctx.Err() != nil {
		return 0, false
	}
	if len(errs) > 0 {
		for _, err := range errs {
			if ctx.Err() != nil {
				return 0, false
			}
			output.PrintErrln("stub error:", err)
		}
		return 0, false
	}
	if ctx.Err() != nil {
		return 0, false
	}
	store.Replace(stubs)
	if announce && ctx.Err() == nil {
		output.Printf("simulacra: %d stub(s) reloaded\n", len(stubs))
	}
	return len(stubs), true
}
