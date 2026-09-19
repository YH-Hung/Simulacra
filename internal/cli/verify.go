package cli

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newVerifyCmd() *cobra.Command { return newVerifyCmdWithClient(newAdminClient) }

func newVerifyCmdWithClient(newClient clientFactory) *cobra.Command {
	var method, matchFile string
	var timeSpecs []string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Assert what a running server was asked to do",
		Long: "Assert what a running server was asked to do.\n\n" +
			"Exits 0 when the assertion holds, 1 when it does not, and 2 when it " +
			"could not run — so CI can tell a failed test from a broken job.",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			if len(timeSpecs) == 0 {
				return errors.New("--times is required, e.g. --times exactly=2")
			}
			times, err := parseTimes(timeSpecs)
			if err != nil {
				return err
			}
			var matcher []byte
			if matchFile != "" {
				if matcher, err = os.ReadFile(matchFile); err != nil {
					return fmt.Errorf("reading %s: %w", matchFile, err)
				}
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Verify.VerifyCalls(callCtx, connect.NewRequest(&adminv1.VerifyCallsRequest{
				Method:          method,
				MatcherDocument: string(matcher),
				Times:           times,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				if err := writeJSON(cmd.OutOrStdout(), resp.Msg); err != nil {
					return err
				}
			} else {
				// The verdict is this command's payload — §5 rests on it
				// being printed before the exit code is set — so it goes to
				// stdout explicitly rather than through cmd.Print*, which
				// routes to os.Stderr in the real binary (design §4).
				payload := newPayloadWriter(cmd)
				payload.printf("%s\n", resp.Msg.GetExplanation())
				for _, actual := range resp.Msg.GetActual() {
					payload.printf("%s\n", callLine(actual.GetCall()))
					payload.printf("  %s\n", actual.GetNearestMiss())
				}
				// A verdict that could not be written is an operational
				// failure, not a verdict: returning it here means exit 2,
				// where falling through to the check below would report 1 —
				// "the assertion ran and failed" — to a CI job that received
				// no verdict at all. A verdict that *was* written keeps the
				// assertion's own result, which is why this is checked before
				// passed rather than after.
				if payload.err != nil {
					return payload.err
				}
			}
			if !resp.Msg.GetPassed() {
				// The verdict is already printed; this only sets the exit
				// code, and Execute suppresses the error: prefix for it.
				return errAssertionFailed
			}
			return nil
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "the method to assert on")
	cmd.Flags().StringVar(&matchFile, "match-file", "",
		"stub-grammar match block; omitted matches any call to the method")
	cmd.Flags().StringArrayVar(&timeSpecs, "times", nil,
		"how many calls must match: exactly=N, at-least=N, at-most=N, or never (repeatable, comma-separated)")
	return cmd
}

// parseTimes builds the Times assertion from --times specs.
//
// Keys accumulate across occurrences and comma-separated groups, so
// `--times at-least=1 --times at-most=3` and `--times at-least=1,at-most=3`
// are the same assertion. A key given twice is an error naming it rather than
// last-one-wins: a caller assembling flags from two places would otherwise get
// a verdict it did not ask for, silently.
//
// This validates only what the CLI alone can see — key spelling and integer
// syntax. Whether a combination is legal is journal.Times.Validate's, and the
// server already calls it; duplicating those rules here is how two surfaces
// drift.
func parseTimes(specs []string) (*adminv1.Times, error) {
	times := &adminv1.Times{}
	// One table for the counted keys, so which keys exist and where their
	// values land cannot drift apart as two switches would.
	counts := map[string]**int32{
		"exactly":  &times.Exactly,
		"at-least": &times.AtLeast,
		"at-most":  &times.AtMost,
	}
	seen := map[string]bool{}
	for _, spec := range specs {
		for _, part := range strings.Split(spec, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key, value, hasValue := strings.Cut(part, "=")
			// Membership is settled before the spec's shape. Asking whether a
			// key carries a value first turns `--times bogus` into "needs a
			// value, e.g. bogus=2" — advice recommending a spelling that is
			// itself rejected, which sends the user one round trip further
			// from the real problem.
			if key == "" {
				return nil, fmt.Errorf(
					"--times %q has no key; use exactly=N, at-least=N, at-most=N, or never", part)
			}
			target, counted := counts[key]
			if !counted && key != "never" {
				return nil, fmt.Errorf(
					"--times %s is not a known key; use exactly=N, at-least=N, at-most=N, or never", key)
			}
			if seen[key] {
				return nil, fmt.Errorf("--times %s given more than once", key)
			}
			seen[key] = true

			if key == "never" {
				if hasValue {
					return nil, errors.New("--times never takes no value")
				}
				times.Never = true
				continue
			}
			// An empty value is caught here rather than left to ParseInt: its
			// `parsing "": invalid syntax` describes the library's problem,
			// not the user's.
			if !hasValue || value == "" {
				return nil, fmt.Errorf("--times %s needs a count, e.g. %s=2", key, key)
			}
			n, err := parseTimesValue(key, value)
			if err != nil {
				return nil, err
			}
			*target = &n
		}
	}
	return times, nil
}

// parseTimesValue parses one count as int32.
//
// ParseInt with a 32-bit size, never Atoi: the wire fields are int32, and on a
// 64-bit host Atoi followed by an int32 conversion wraps silently — 2147483648
// becomes -2147483648, sending the server a different assertion than the user
// wrote, under the user's name.
func parseTimesValue(key, value string) (int32, error) {
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("--times %s=%s: %w", key, value, err)
	}
	return int32(n), nil
}
