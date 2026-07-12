package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func newServeCmd() *cobra.Command {
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
			lis, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("listening on %s: %w", listen, err)
			}

			cmd.Printf("simulacra: data plane listening on %s\n", lis.Addr())
			cmd.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				len(reg.Services()), len(stubs))
			watcher := watcherRun{}
			if watchStubs && len(src.stubDirs) > 0 {
				watcher = startWatcher(cmd.Context(), func(ctx context.Context) error {
					return stub.Watch(ctx, src.stubDirs, 200*time.Millisecond, func() {
						reloadStubDirs(cmd, reg, store, src.stubDirs)
					})
				}, func(err error) {
					cmd.PrintErrln("watch error:", err)
				})
			}

			sig := make(chan os.Signal, 2)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go waitAndShutdown(sig, 10*time.Second, func() {
				watcher.cancelNow()
				srv.GracefulStop()
			}, func() {
				watcher.cancelNow()
				srv.Stop()
			}, cmd.Println)
			return serveWithWatcher(watcher, func() error { return srv.Serve(lis) })
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

func startWatcher(parent context.Context, run func(context.Context) error, report func(error)) watcherRun {
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := run(ctx); err != nil {
			report(err)
		}
	}()
	return watcherRun{cancel: cancel, done: done}
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

func serveWithWatcher(watcher watcherRun, serve func() error) error {
	defer watcher.stop()
	return serve()
}

func reloadStubDirs(cmd *cobra.Command, reg *schema.Registry, store *stub.Store, dirs []string) {
	stubs, errs := stub.LoadDirs(reg, dirs)
	if len(errs) > 0 {
		for _, err := range errs {
			cmd.PrintErrln("stub error:", err)
		}
		return
	}
	store.Replace(stubs)
	cmd.Printf("simulacra: %d stub(s) reloaded\n", len(stubs))
}
