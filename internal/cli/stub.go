package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func newStubCmd() *cobra.Command { return newStubCmdWithClient(newAdminClient) }

func newStubCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stub",
		Short: "Inspect and manage the stubs a running server is serving",
	}
	cmd.AddCommand(
		newStubListCmd(newClient),
		newStubRemoveCmd(newClient),
		newStubExportCmd(newClient),
		newStubAddCmd(newClient),
	)
	return cmd
}

func newStubListCmd(newClient clientFactory) *cobra.Command {
	var method, origin string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List stubs",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := out.validate(); err != nil {
				return err
			}
			originEnum, err := parseOrigin(origin)
			if err != nil {
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

			resp, err := client.Stub.ListStubs(callCtx, connect.NewRequest(&adminv1.ListStubsRequest{
				Method: method,
				Origin: originEnum,
			}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			stubs := resp.Msg.GetStubs()
			if len(stubs) == 0 {
				cmd.PrintErrln("no stubs")
				return nil
			}
			table := newTable(cmd.OutOrStdout())
			fmt.Fprintln(table, "ID\tMETHOD\tORIGIN\tHITS\tTIMES")
			for _, s := range stubs {
				fmt.Fprintf(table, "%s\t%s\t%s\t%d\t%s\n",
					s.GetId(), s.GetMethod(), originName(s.GetOrigin()),
					s.GetHits(), timesText(s.GetTimes()))
			}
			return table.Flush()
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVar(&method, "method", "", "only stubs for this method")
	cmd.Flags().StringVar(&origin, "origin", "", `only stubs of this origin: "file" or "api"`)
	return cmd
}

func newStubRemoveCmd(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:         "rm <id>...",
		Short:       "Remove stubs by id",
		Args:        cobra.MinimumNArgs(1),
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, args []string) error {
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()

			// Fail fast, and do not roll back: a removed stub cannot be
			// recreated with its ID, so there is no prior state to restore.
			// What the command owes the user is an accurate account of what it
			// did before it stopped.
			payload := newPayloadWriter(cmd)
			for _, id := range args {
				callCtx, cancel := client.callContext(ctx)
				_, err := client.Stub.DeleteStub(callCtx,
					connect.NewRequest(&adminv1.DeleteStubRequest{Id: id}))
				cancel()
				if err != nil {
					// rpcError first, then the id: rpcError replaces an
					// interrupted call's transport error with errInterrupted,
					// so wrapping before it would throw the id away in exactly
					// the case the user most needs it — knowing which delete
					// was in flight when the signal landed.
					return fmt.Errorf("%s: %w", id, rpcError(ctx, err))
				}
				// What this invocation did, not commentary about it: the record
				// of which ids are gone is the command's payload (design §4).
				payload.printf("removed %s\n", id)
			}
			// The account is the deliverable, so an account that could not be
			// written is a failed command even though every delete landed. The
			// deletes are not undoable, so the honest report is the non-zero
			// exit, not silence.
			return payload.err
		},
	}
	clientFlags{}.register(cmd)
	return cmd
}

func newStubExportCmd(newClient clientFactory) *cobra.Command {
	var outPath string
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "export",
		Short: "Export API-origin stubs as a stub file",
		Long: "Export API-origin stubs as one file-grammar YAML sequence.\n\n" +
			"--output chooses what is written, --out chooses where it goes; the two compose.",
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

			resp, err := client.Stub.ExportStubs(callCtx,
				connect.NewRequest(&adminv1.ExportStubsRequest{}))
			if err != nil {
				return rpcError(ctx, err)
			}
			render := func(w io.Writer) error {
				if out.json() {
					return writeJSON(w, resp.Msg)
				}
				_, err := io.WriteString(w, resp.Msg.GetDocument())
				return err
			}
			if err := writeOut(cmd, outPath, render); err != nil {
				return err
			}
			// Commentary goes to stderr so a piped or redirected document
			// carries only the payload.
			cmd.PrintErrf("%d stub(s)\n", resp.Msg.GetStubCount())
			return nil
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	cmd.Flags().StringVarP(&outPath, "out", "o", "",
		"write to this file instead of stdout")
	return cmd
}

// writeOut sends render's output to path, or to the command's stdout when path
// is empty. The file is created with the 0666 os.Create requests, which the
// process umask then narrows, so an exported stub file is readable by the
// tooling that will load it back.
func writeOut(cmd *cobra.Command, path string, render func(io.Writer) error) error {
	if path == "" {
		return render(cmd.OutOrStdout())
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}
	if err := render(file); err != nil {
		file.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}
	return nil
}

// parseOrigin maps the --origin flag onto the filter enum. An empty flag means
// every origin.
func parseOrigin(text string) (adminv1.StubOrigin, error) {
	switch text {
	case "":
		return adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED, nil
	case "file":
		return adminv1.StubOrigin_STUB_ORIGIN_FILE, nil
	case "api":
		return adminv1.StubOrigin_STUB_ORIGIN_API, nil
	}
	return adminv1.StubOrigin_STUB_ORIGIN_UNSPECIFIED,
		fmt.Errorf(`--origin must be "file" or "api" (got %q)`, text)
}

func originName(o adminv1.StubOrigin) string {
	switch o {
	case adminv1.StubOrigin_STUB_ORIGIN_FILE:
		return "file"
	case adminv1.StubOrigin_STUB_ORIGIN_API:
		return "api"
	default:
		return "unknown"
	}
}

// timesText renders a use budget. Zero means unlimited in the file grammar,
// and the table says so rather than printing a bare 0 the reader must decode.
func timesText(times int32) string {
	if times == 0 {
		return "unlimited"
	}
	return strconv.Itoa(int(times))
}

// rollbackTimeout bounds compensation. It is deliberately short: the work is a
// handful of deletes against a server that just answered.
const rollbackTimeout = 10 * time.Second

// pendingStub is one stub document with the source that produced it, so a
// diagnostic can name <file>#<index> rather than an opaque ordinal.
type pendingStub struct {
	source   string
	document string
}

func newStubAddCmd(newClient clientFactory) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:   "add",
		Short: "Create stubs from stub files",
		Long: "Create stubs from stub files.\n\n" +
			"Every file is read, split and parsed before the first request, so a\n" +
			"malformed file creates nothing. If the server rejects a stub, the stubs\n" +
			"this command already created are deleted.",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return errors.New("--file <stub-file> is required")
			}
			// The client is built first because doing so sends nothing: it
			// validates flags and allocates, which is why the zero-RPC
			// guarantee below is unaffected by the order.
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			// The signal context is installed before preflight, not after.
			// Preflight is where the reads happen, and a read can block for as
			// long as its writer likes — `-f -` on a pipe, `-f <fifo>` on a
			// named one — so a signal arriving there with no handler installed
			// kills the process by its default disposition — 130 or 143 —
			// rather than the 2 an interrupted non-tail command owes the
			// caller (design §5). Installing it early is only half of that:
			// readSource is what makes the cancellation reach the read.
			ctx, stop := signalContext(cmd)
			defer stop()

			// Preflight: everything that can be checked without the server is
			// checked before anything is sent, so the largest class of failure
			// — a malformed input file — creates nothing at all.
			pending, err := preflightStubs(ctx, cmd, files)
			if err != nil {
				return err
			}

			payload := newPayloadWriter(cmd)
			created := make([]string, 0, len(pending))
			for _, p := range pending {
				callCtx, cancel := client.callContext(ctx)
				resp, err := client.Stub.CreateStub(callCtx,
					connect.NewRequest(&adminv1.CreateStubRequest{Document: p.document}))
				cancel()
				if err != nil {
					return rollback(cmd, client, created, p, rpcError(ctx, err))
				}
				id := resp.Msg.GetStub().GetId()
				created = append(created, id)
				// The created ids are what the caller came for — a script
				// pipes them straight into `stub rm` — so they are payload,
				// the same judgement `stub rm`'s "removed" line makes
				// (design §4).
				payload.printf("created %s\t%s\n",
					id, resp.Msg.GetStub().GetMethod())
			}
			// The stubs exist; the caller just did not receive their ids. That
			// is a failed command, and the non-zero exit is what says so —
			// `stub list` is where the ids can still be recovered.
			return payload.err
		},
	}
	clientFlags{}.register(cmd)
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil,
		`stub file to create from, or "-" for stdin (repeatable)`)
	return cmd
}

// preflightStubs reads and splits every file before any RPC is sent.
//
// `-` may appear at most once. Stdin is consumed by the first read, so a second
// `-` would yield an empty document set and contribute nothing — the command
// would report success over input it never saw. Rejecting it says so instead.
func preflightStubs(ctx context.Context, cmd *cobra.Command, paths []string) ([]pendingStub, error) {
	var pending []pendingStub
	stdinRead := false
	for _, path := range paths {
		if path == "-" {
			if stdinRead {
				return nil, errors.New(
					`-f - may be given only once: stdin is consumed by the first read, ` +
						`so a second "-" would add nothing`)
			}
			stdinRead = true
		}
		raw, err := readSource(ctx, cmd, path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		// The same splitter the file loader uses, so the CLI and a --stubs
		// directory can never disagree about what one stub is.
		docs, err := stub.SplitDocuments(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		for i, doc := range docs {
			source := fmt.Sprintf("%s#%d", path, i)
			// Splitting proves the file is a list; it does not prove each item
			// is a stub. ParseDocument is the strict decode the server's own
			// compileDocument runs, so checking it here keeps one grammar
			// rather than inventing a second validator — and turns an unknown
			// field from a server rejection into a preflight failure, which is
			// what "no RPC is sent at all" requires (design §6.1).
			//
			// Only the error is wanted: what goes on the wire is the
			// splitter's rendering, not this decode's. And only the local half
			// of the grammar is decidable here — an unknown method or a CEL
			// diagnostic needs the registry, so those stay server-side.
			if _, _, err := stub.ParseDocument([]byte(doc)); err != nil {
				return nil, fmt.Errorf("%s: %w", source, err)
			}
			pending = append(pending, pendingStub{source: source, document: doc})
		}
	}
	return pending, nil
}

// readSource reads one stub source whole, giving up when ctx ends. A path of
// "-" means cmd's stdin; anything else is a file.
//
// A pipe read blocks until the writer closes it, and no context can reach a
// read already inside the syscall — so the read runs on its own goroutine and
// the select is what makes the signal observable at all. Without it, a
// `stub add -f -` waiting on a pipe dies by default disposition even with a
// handler installed, because nothing is watching the context.
//
// A named path gets the same treatment rather than a plain os.ReadFile,
// because "a path names a file that returns promptly" is not something this
// code can know: a FIFO, a device node, or a file on a stalled network mount
// each block exactly as stdin does. With the handler installed before
// preflight, a synchronous read there is worse than no handler at all — the
// signal is caught, the context is cancelled, and the read goes on blocking,
// so the command hangs and then reports success over input it never saw. No
// stat can sort the two cases out either: a network mount looks like a regular
// file, and the answer is a property of the read, not of the inode. So the
// rule is applied uniformly, which costs a regular file only the goroutine its
// already-finished read returns through.
//
// On cancellation that goroutine is left blocked, holding stdin or an open file
// descriptor, which is deliberate: the process is on its way out, the buffered
// channel means a late read cannot block it forever, and closing the file to
// unblock it would race the very read it is trying to interrupt, for no gain.
func readSource(ctx context.Context, cmd *cobra.Command, path string) ([]byte, error) {
	type result struct {
		raw []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		var r result
		if path == "-" {
			r.raw, r.err = io.ReadAll(cmd.InOrStdin())
		} else {
			r.raw, r.err = os.ReadFile(path)
		}
		done <- r
	}()
	select {
	case r := <-done:
		// A cancellation already visible when the read returns still wins:
		// select chooses at random when both cases are ready, so without this
		// an interrupt could be discarded in favour of a read that finished at
		// the same moment. It is the same rule rpcError applies to a failed
		// call — once the signal context is done, the command was interrupted
		// whatever else is also true.
		//
		// It is also what keeps the interrupted read from being mistaken for an
		// empty one. A FIFO whose writer closes after the signal hands back
		// zero bytes and no error, which is indistinguishable from an empty
		// stub file — so without this check the command would add nothing and
		// exit 0, reporting success for a run the operator cut short.
		//
		// It does not win a signal still in flight, and nothing here could:
		// delivery is asynchronous, so a source that reached EOF just before
		// the handler ran is a complete read and the command finishes
		// normally. What this removes is the coin flip, which is the only part
		// of that race this code decides.
		if ctx.Err() != nil {
			return nil, errInterrupted
		}
		return r.raw, r.err
	case <-ctx.Done():
		return nil, errInterrupted
	}
}

// rollback compensates for a failed create by deleting what this invocation
// made, newest first, then returns the error the command reports.
//
// This is compensation, not a transaction, and the wording never pretends
// otherwise. CreateStub mutates the store before it returns, so a response
// lost to a dropped connection, a deadline, or an interrupt leaves a stub whose
// id the client never learned — unnameable and therefore undeletable. True
// atomicity would need a server-side batch RPC or an idempotency token.
//
// So the report is built from two separate questions, not one: whether the
// compensating deletes all landed, and whether the failed create itself could
// have stored something. createRejected answers the second, and only a yes
// there earns the words "no stubs were added".
//
// The deletes run under their own bounded context rather than the command's.
// Compensation must still run when the command context is already cancelled or
// past its deadline, which is exactly what a lost or timed-out create produces;
// a rollback inheriting that context would be dead before it sent a byte.
func rollback(cmd *cobra.Command, client *adminClient, created []string, failed pendingStub, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), rollbackTimeout)
	defer cancel()

	var orphaned []string
	for i := len(created) - 1; i >= 0; i-- {
		id := created[i]
		if _, err := client.Stub.DeleteStub(ctx,
			connect.NewRequest(&adminv1.DeleteStubRequest{Id: id})); err != nil {
			orphaned = append(orphaned, id)
		}
	}
	if len(orphaned) > 0 {
		cmd.PrintErrf(
			"rollback incomplete: %d stub(s) could not be removed and are still installed: %s\n",
			len(orphaned), strings.Join(orphaned, ", "))
	}
	// Two independent facts, and the compensating deletes only settle the
	// first. Whether the failed create itself left a stub behind is decided by
	// what went wrong, not by whether the deletes succeeded — which is why
	// "no stubs were added" is claimed only when the server's own verdict says
	// the document was never stored.
	switch {
	case !createRejected(cause):
		cmd.PrintErrf(
			"the store may hold a stub this command could not identify: %s was in flight "+
				"when the call failed; check `simulacra stub list` before retrying\n",
			failed.source)
	case len(orphaned) == 0:
		cmd.PrintErrln("no stubs were added")
	}
	return fmt.Errorf("%s: %w", failed.source, cause)
}

// createRejected reports whether cause is the server's verdict on the document
// — a decision reached before anything was stored — as opposed to a failure
// that leaves the outcome unknown.
//
// The distinction is what separates a rollback that is complete from one that
// only looks complete. CreateStub mutates the store before it returns
// (internal/admin/stub.go: Store.Add, then the response), so a deadline, a
// dropped connection, a cancellation, or an interrupt can all leave a stub
// installed whose id never reached the client. Nothing here can delete it, and
// nothing here may pretend it does not exist (design §6.1).
//
// Only the codes the handler reaches by refusing the document count as a
// verdict. Everything else — DEADLINE_EXCEEDED, CANCELED, UNAVAILABLE, the
// UNKNOWN a non-connect error reports, and errInterrupted, which rpcError
// substitutes for whatever the transport said once the signal context is done
// — leaves the outcome unknown. INTERNAL is deliberately outside the verdict
// set: internal/admin/errors.go returns it for anything it could not classify,
// so it is not a statement about this document, and an interceptor or a proxy
// can produce it without the handler having run at all. Being wrong in that
// direction costs a warning the user can check; being wrong the other way
// costs them a stub they were told did not exist.
func createRejected(cause error) bool {
	if errors.Is(cause, errInterrupted) {
		return false
	}
	switch connect.CodeOf(cause) {
	case connect.CodeInvalidArgument, connect.CodeNotFound,
		connect.CodeFailedPrecondition, connect.CodeAlreadyExists:
		return true
	default:
		return false
	}
}
