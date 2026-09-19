// Package cli wires the simulacra command-line interface.
package cli

import (
	"errors"
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
	root.AddCommand(
		newServeCmd(),
		newCheckCmd(),
		newStubCmd(),
		newCallsCmd(),
		newVerifyCmd(),
		newSchemaCmd(),
	)
	return root
}

func Execute() {
	root := newRootCmd()
	cmd, err := root.ExecuteC()
	// A failed assertion has already printed its verdict; it is the command's
	// output, not a diagnostic about the command (design §5).
	if err != nil && !errors.Is(err, errAssertionFailed) {
		root.PrintErrln("error:", err)
	}
	if code := exitCode(cmd, err); code != 0 {
		os.Exit(code)
	}
}
