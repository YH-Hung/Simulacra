package cli

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/server"
)

func newServeCmd() *cobra.Command {
	return newServeCmdWithListen(net.Listen)
}

func newServeCmdWithListen(serveListen func(string, string) (net.Listener, error)) *cobra.Command {
	src := &sources{}
	var listen string
	var admin string
	var journalSize int
	var watchStubs bool
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mock gRPC server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if journalSize <= 0 {
				return fmt.Errorf("journal-size must be greater than zero (got %d)", journalSize)
			}
			output := &commandOutput{cmd: cmd}
			srv, err := server.Start(cmd.Context(), server.Options{
				ProtoDirs:          src.protoDirs,
				DescriptorSetPaths: src.descriptorSets,
				StubDirs:           src.stubDirs,
				DataAddr:           listen,
				AdminAddr:          adminAddrOption(admin),
				JournalSize:        journalSize,
				Watch:              watchStubs,
				Listen:             serveListen,
				Reporter:           output,
			})
			if err != nil {
				return err
			}

			output.Printf("simulacra: data plane listening on %s\n", srv.DataAddr())
			output.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				srv.ServiceCount(), srv.StubCount())
			if addr := srv.AdminAddr(); addr != nil {
				output.Printf("simulacra: admin plane listening on %s\n", addr)
			}

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
				waitAndShutdownContextStop(cmd.Context(), stopShutdownWaiter, sig, 10*time.Second,
					srv.GracefulStop, srv.Stop, output.Println)
			}()
			return serveWithRuntime(cancelShutdown, shutdownDone, srv.Wait)
		},
	}
	src.register(cmd)
	cmd.Flags().StringVar(&listen, "listen", ":6565", "data-plane listen address")
	cmd.Flags().StringVar(&admin, "admin", "127.0.0.1:6566",
		`admin-plane listen address, or "off" to disable the control plane. `+
			`Loopback only by default — unauthenticated, it must not be reachable `+
			`from other hosts unless asked for; pass an explicit address such as `+
			`"0.0.0.0:6566" to allow that.`)
	cmd.Flags().IntVar(&journalSize, "journal-size", 1024, "number of recent data-plane calls to retain")
	cmd.Flags().BoolVar(&watchStubs, "watch", true, "watch stub directories and reload changes")
	return cmd
}

func serveWithRuntime(cancelShutdown context.CancelFunc, shutdownDone <-chan struct{}, serve func() error) error {
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

// adminAddrOption maps the --admin flag onto server.Options.AdminAddr. One
// string carries both the address and the on/off decision, reading the way
// --listen already does; "off" is the documented opt-out from the default
// admin plane.
func adminAddrOption(flag string) string {
	if flag == "off" {
		return ""
	}
	return flag
}
