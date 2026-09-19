package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

// errAssertionFailed marks a verdict of "no" — the assertion ran and did not
// hold. It is distinct from every operational failure so that a CI job can
// tell a failed test from a broken one (design §5). Only `verify` returns it.
var errAssertionFailed = errors.New("assertion failed")

// errInterrupted is what a client command other than `calls tail` returns when
// a signal cut it short. The work did not finish, so reporting success would
// tell a script otherwise (design §5).
var errInterrupted = errors.New("interrupted before completion")

const (
	// exitAnnotation scopes the 0/1/2 exit contract to the admin-plane client
	// commands. Commands without it — serve, check, the root — keep exiting 1
	// on any error, so no shipped command changes its exit code.
	exitAnnotation = "exit"
	exitClient     = "client"
)

// clientAnnotations is the annotation map every client command sets.
// Setting it in one place is what keeps a later command from silently
// inheriting the wrong exit policy by forgetting the literal.
func clientAnnotations() map[string]string {
	return map[string]string{exitAnnotation: exitClient}
}

// exitCode maps the command that ran and the error it returned onto a process
// exit status. cmd is whatever ExecuteC resolved, which is the found
// subcommand even when its flags failed to parse, and nil is tolerated.
func exitCode(cmd *cobra.Command, err error) int {
	if err == nil {
		return 0
	}
	if cmd == nil || cmd.Annotations[exitAnnotation] != exitClient {
		return 1
	}
	if errors.Is(err, errAssertionFailed) {
		return 1
	}
	return 2
}
