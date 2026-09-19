package cli

import (
	"errors"
	"fmt"
	"testing"

	"github.com/spf13/cobra"
)

// The exit contract (design §5): 0 success, 1 assertion failed, 2 operational
// error — and the table governs annotated client commands only, so serve and
// check keep exiting 1 on any error.
func TestExitCodeContract(t *testing.T) {
	client := &cobra.Command{Use: "verify", Annotations: clientAnnotations()}
	plain := &cobra.Command{Use: "serve"}

	cases := []struct {
		name string
		cmd  *cobra.Command
		err  error
		want int
	}{
		{"client success", client, nil, 0},
		{"client assertion failure", client, errAssertionFailed, 1},
		{"client wrapped assertion failure", client, fmt.Errorf("verify: %w", errAssertionFailed), 1},
		{"client operational error", client, errors.New("connection refused"), 2},
		{"client interrupted", client, errInterrupted, 2},
		{"plain command error stays 1", plain, errors.New("boom"), 1},
		{"plain command success", plain, nil, 0},
		{"nil command error stays 1", nil, errors.New("unknown command"), 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCode(tc.cmd, tc.err); got != tc.want {
				t.Fatalf("exitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

// A typo'd flag on verify must not be reported as a failed assertion: it did
// not run. ExecuteC carries the subcommand's annotations through a flag error,
// which is what makes this reachable.
func TestFlagErrorOnClientCommandExitsTwo(t *testing.T) {
	sub := &cobra.Command{
		Use:         "verify",
		Annotations: clientAnnotations(),
		RunE:        func(*cobra.Command, []string) error { return nil },
	}
	sub.Flags().String("times", "", "")
	root := &cobra.Command{Use: "simulacra", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(sub)
	root.SetArgs([]string{"verify", "--tiems", "exactly=2"})

	cmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("expected a flag error")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2 — a typo'd flag is not a failed assertion", got)
	}
}

// An unknown top-level command resolves to the root, which carries no
// annotation, so it keeps the pre-existing exit 1.
func TestUnknownCommandExitsOne(t *testing.T) {
	root := &cobra.Command{Use: "simulacra", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(&cobra.Command{Use: "verify", Annotations: clientAnnotations()})
	root.SetArgs([]string{"bogus"})

	cmd, err := root.ExecuteC()
	if err == nil {
		t.Fatal("expected an unknown-command error")
	}
	if got := exitCode(cmd, err); got != 1 {
		t.Fatalf("exitCode = %d, want 1", got)
	}
}
