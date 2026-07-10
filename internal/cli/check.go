package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	src := &sources{}
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate stub files against schemas without starting a server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(src.stubDirs) == 0 {
				return errors.New("--stubs <dir> is required")
			}
			reg, err := src.buildRegistry(cmd.Context())
			if err != nil {
				return err
			}
			stubs, err := src.loadStubs(cmd, reg)
			if err != nil {
				return err
			}
			cmd.Printf("OK: %d stub(s) validated against %d service(s)\n",
				len(stubs), len(reg.Services()))
			return nil
		},
	}
	src.register(cmd)
	return cmd
}
