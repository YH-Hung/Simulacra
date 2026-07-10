// Package cli wires the simulacra command-line interface.
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "simulacra",
		Short:         "A gRPC-native mock server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newCheckCmd())
	return root
}

func Execute() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		root.PrintErrln("error:", err)
		os.Exit(1)
	}
}
