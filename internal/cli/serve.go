package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func newServeCmd() *cobra.Command {
	src := &sources{}
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mock gRPC server",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := src.buildRegistry(cmd.Context())
			if err != nil {
				return err
			}
			stubs, err := src.loadStubs(cmd, reg)
			if err != nil {
				return err
			}
			srv, err := dataplane.New(reg, stub.NewStore(stubs))
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

			sig := make(chan os.Signal, 2)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			defer signal.Stop(sig)
			go waitAndShutdown(sig, 10*time.Second, srv.GracefulStop, srv.Stop, cmd.Println)
			return srv.Serve(lis)
		},
	}
	src.register(cmd)
	cmd.Flags().StringVar(&listen, "listen", ":6565", "data-plane listen address")
	return cmd
}
