package cli

import (
	"context"
	"fmt"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newCallsCmd() *cobra.Command { return newCallsCmdWithClient(newAdminClient) }

func newCallsCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "calls",
		Short: "Inspect the calls a running server has recorded",
	}
	cmd.AddCommand(newCallsListCmd(newClient), newCallsTailCmd(newClient))
	return cmd
}

func newCallsListCmd(newClient clientFactory) *cobra.Command {
	var method string
	var limit int32
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List recorded calls, newest first",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Journal.ListCalls(callCtx, connect.NewRequest(&adminv1.ListCallsRequest{
				Method: method,
				Limit:  limit,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			calls := resp.Msg.GetCalls()
			if len(calls) == 0 {
				cmd.PrintErrln("no calls")
				return nil
			}
			table := newTable(cmd.OutOrStdout())
			fmt.Fprintln(table, "SEQ\tMETHOD\tCODE\tDURATION\tSTUB")
			for _, c := range calls {
				fmt.Fprintln(table, callLine(c))
			}
			return table.Flush()
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only calls to this method")
	cmd.Flags().Int32Var(&limit, "limit", 0, "return at most this many calls; 0 means no limit")
	return cmd
}

// callLine is the one-line summary `calls list` prints. Full detail belongs to
// --output json: a 1024-call journal rendered in full is not a readable
// default.
func callLine(c *adminv1.Call) string {
	return fmt.Sprintf("%d\t%s\t%d\t%s\t%s",
		c.GetSeq(), c.GetMethod(), c.GetStatus().GetCode(),
		c.GetDuration().AsDuration(), c.GetMatchedStubId())
}

// callStream is the part of a WatchCalls stream tailLoop uses. Narrowing it to
// an interface is what lets the stream-end policy be tested without
// reproducing an eviction, the same separation 4b made for streamCalls.
type callStream interface {
	Receive() bool
	Msg() *adminv1.WatchCallsResponse
	Err() error
	Close() error
}

func newCallsTailCmd(newClient clientFactory) *cobra.Command {
	var method string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "tail",
		Short:       "Stream calls as they are recorded",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()

			open := func(ctx context.Context) (callStream, error) {
				return client.Journal.WatchCalls(ctx,
					connect.NewRequest(&adminv1.WatchCallsRequest{Method: method}))
			}
			emit := func(c *adminv1.Call) error {
				if out.json() {
					return writeJSONLine(cmd.OutOrStdout(), &adminv1.WatchCallsResponse{Call: c})
				}
				// The streamed calls are the payload, so they go to stdout
				// explicitly: cmd.Printf routes through OutOrStderr, which
				// falls back to os.Stderr in the real binary (design §4).
				payload := newPayloadWriter(cmd)
				payload.printf("%s\n", callLine(c))
				for _, req := range c.GetRequests() {
					payload.printf("  %s\n", req.GetJson())
				}
				return payload.err
			}
			warn := func(msg string) { cmd.PrintErrln(msg) }

			return tailLoop(ctx, open, emit, warn)
		},
	}
	// No --timeout: the stream is unbounded by design and ends on a signal.
	clientFlags{}.registerNoTimeout(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only calls to this method")
	return cmd
}

// tailLoop streams calls until the context ends or a terminal error arrives.
//
// Eviction is the one ending that resumes. RESOURCE_EXHAUSTED means the
// journal dropped this consumer for falling behind; a fresh watch starts from
// now, so the calls in the gap are gone and the warning says so rather than
// implying a gapless stream.
//
// UNAVAILABLE is terminal, and deliberately not distinguished by message text.
// Server teardown reports "server shutting down" and an unreachable address
// reports a dial failure, but both are UNAVAILABLE — matching on the wording to
// tell them apart would couple the CLI to the server's phrasing, which is the
// opposite of surfacing diagnostics verbatim. Both deserve the same answer:
// the server is gone. Reconnecting would busy-loop against a server that is
// tearing down, and a tail that silently survived a restart would be reporting
// on a journal that no longer exists.
func tailLoop(
	ctx context.Context,
	open func(context.Context) (callStream, error),
	emit func(*adminv1.Call) error,
	warn func(string),
) error {
	for {
		if ctx.Err() != nil {
			return nil
		}
		stream, err := open(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		for stream.Receive() {
			if err := emit(stream.Msg().GetCall()); err != nil {
				stream.Close()
				return err
			}
		}
		err = stream.Err()
		stream.Close()

		if ctx.Err() != nil {
			return nil
		}
		if connect.CodeOf(err) == connect.CodeResourceExhausted {
			warn("simulacra: the journal dropped this watch for falling behind; " +
				"resuming from now — calls recorded in the gap are lost")
			continue
		}
		return err
	}
}
