package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	"github.com/yinghanhung/simulacra/internal/schema/upstream"
)

// importDefaultTimeout bounds the whole walk, not one RPC — the walk can
// legitimately make many round trips, and the CLI has no way to tell "the
// nth RPC is slow" from "the (n+1)th is coming". A closed port fails fast on
// its own; this is for an upstream that accepts the connection and never
// answers.
const importDefaultTimeout = 30 * time.Second

func newSchemaImportCmd() *cobra.Command {
	var (
		reflectAddr string
		plaintext   bool
		outPath     string
		timeout     time.Duration
	)
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:   "import",
		Short: "Import a descriptor set from a running server's reflection endpoint",
		// import is under the 0/1/2 exit contract but is NOT an admin-plane
		// client: it dials an upstream, never our admin plane. So it carries
		// the annotation without clientFlags -- --addr would be meaningless
		// here, and SIMULACRA_ADDR must not silently retarget an import
		// (design §6). It registers its own --timeout below, bounding the
		// whole walk rather than one RPC.
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if reflectAddr == "" {
				return errors.New("--reflect <host:port> is required")
			}
			if outPath == "" {
				return errors.New("--out <path> is required")
			}
			if err := out.validate(); err != nil {
				return err
			}
			if timeout < 0 {
				return fmt.Errorf("--timeout must not be negative (got %s)", timeout)
			}

			// Signal context first, before dialing and before any file
			// exists: the phase 5 lesson is to establish the command's
			// guarantees before the first side effect. The timeout is
			// derived from it, not the other way around, so an interrupt
			// still wins over a deadline that happens to land at the same
			// moment.
			ctx, stop := signalContext(cmd)
			defer stop()
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			cc, err := upstream.Dial(reflectAddr, plaintext)
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", reflectAddr, err)
			}
			defer func() { _ = cc.Close() }()

			fetcher, release := upstream.NewFetcher(upstream.OpenerFor(cc))
			defer release()
			set, err := upstream.Closure(ctx, fetcher)
			if err != nil {
				if ctxErr := walkEnded(ctx, timeout, "importing from "+reflectAddr); ctxErr != nil {
					return ctxErr
				}
				return fmt.Errorf("importing from %s: %w", reflectAddr, err)
			}
			raw, err := proto.Marshal(set)
			if err != nil {
				return fmt.Errorf("encoding the imported descriptor set: %w", err)
			}
			if err := publishImport(ctx, timeout, outPath, raw); err != nil {
				return err
			}
			// An upstream where every advertised service is one Simulacra's
			// data plane implements itself (health, reflection) -- a
			// health-only sidecar, or the wrong port -- produces a genuinely
			// empty set. That is a usable, predictable outcome, not a
			// failure: the file is still written and the exit code stays 0,
			// matching `schema list`'s own "no services registered" note
			// (internal/cli/schema.go). Silence here would read as success
			// against a target that imported nothing useful.
			if len(set.GetFile()) == 0 {
				cmd.PrintErrf("no services were importable from %s; services "+
					"the data plane implements itself (health, reflection) "+
					"are always skipped\n", reflectAddr)
			}

			files := make([]string, 0, len(set.GetFile()))
			for _, f := range set.GetFile() {
				files = append(files, f.GetName())
			}
			return writeImportSummary(cmd, out.json(), files, countServices(set), len(raw), outPath)
		},
	}
	cmd.Flags().StringVar(&reflectAddr, "reflect", "",
		"address of the server to import from, e.g. staging.internal:443")
	cmd.Flags().BoolVar(&plaintext, "plaintext", false,
		"connect without TLS")
	cmd.Flags().StringVarP(&outPath, "out", "o", "",
		"write the descriptor set here")
	cmd.Flags().DurationVar(&timeout, "timeout", importDefaultTimeout,
		"deadline for the whole walk; 0 disables it")
	out.register(cmd)
	return cmd
}

// publish writes raw to path atomically.
//
// os.Rename is the commit boundary and it is NOT context-aware: it will
// happily publish after the walk's context has been cancelled, so atomic
// replacement alone does not give cancellation safety. The explicit check
// below narrows that window; it cannot close it, and past the rename a
// successful publication has intentionally replaced the destination
// (design §6).
func publish(ctx context.Context, path string, raw []byte) error {
	return publishObserved(ctx, path, raw, nil)
}

// publishObserved is publish with an optional hook, called with the temporary
// file's name once it holds the complete replacement contents and before its
// final mode is applied. Production passes nil. The hook exists so a test can
// inspect that window directly: it is where the contents must not be readable
// by anyone the destination's own mode would exclude, and a check on the
// published file alone cannot see it.
func publishObserved(ctx context.Context, path string, raw []byte, written func(tmpName string)) error {
	dir := filepath.Dir(path)
	// Private from creation, not narrowed afterwards: permission checks happen
	// at open(2), so a reader who opened the file while it was briefly
	// permissive would keep a readable descriptor after any later chmod and
	// could read the contents once they were written.
	tmp, err := createExclusive(dir, filepath.Base(path), "tmp", 0o600)
	if err != nil {
		return fmt.Errorf("creating a temporary file beside %s: %w", path, err)
	}
	tmpName := tmp.Name()
	// Harmless after a successful rename, and the cleanup path for every
	// failure from here on.
	defer func() { _ = os.Remove(tmpName) }()

	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("writing %s: %w", tmpName, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmpName, err)
	}
	if written != nil {
		written(tmpName)
	}

	// Only now, with the contents complete, does the file take its final
	// mode. os.Rename publishes tmpName's inode as-is, so this is the mode the
	// destination ends up with. Widening it here exposes nothing the
	// destination's own mode would not: that is the mode being published.
	mode, err := finalMode(path)
	if err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", tmpName, err)
	}

	if ctx.Err() != nil {
		// publish does not know the timeout, so it cannot tell a deadline
		// from a signal; it reports the signal sentinel and the caller,
		// which does know, translates a deadline into a timeout message.
		return errInterrupted
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("publishing %s: %w", path, err)
	}
	return nil
}

// finalMode is the mode the published file should carry.
//
// An existing destination keeps its own mode; os.Rename would otherwise
// replace it with the temporary file's. A destination that does not exist yet
// gets the mode any newly created file gets. Any other failure to read the
// existing destination is an error rather than a guess: falling back to the
// new-file mode could publish contents more widely than the file being
// replaced allowed.
func finalMode(path string) (os.FileMode, error) {
	info, err := os.Stat(path)
	if err == nil {
		return info.Mode().Perm(), nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return 0, fmt.Errorf("reading the mode of the existing %s: %w", path, err)
	}
	return newFileMode(filepath.Dir(path), filepath.Base(path))
}

// newFileMode reports the mode a file newly created in dir gets by default:
// 0644 narrowed by the process umask, and by a default ACL where the
// filesystem applies one.
//
// It learns this by creating and immediately removing an empty probe, which
// lets the kernel apply the umask rather than this process reading it --
// syscall.Umask is process-global and racy to read in a multi-goroutine
// program. The probe is a separate file on purpose. The temporary file cannot
// be created this way, because it would be permissive for as long as it took
// to narrow it, and a reader who opened it in that window would keep the
// descriptor. The probe never holds anything, so opening it gains nothing.
func newFileMode(dir, base string) (os.FileMode, error) {
	probe, err := createExclusive(dir, base, "probe", 0o644)
	if err != nil {
		return 0, fmt.Errorf("probing the default file mode in %s: %w", dir, err)
	}
	name := probe.Name()
	info, statErr := probe.Stat()
	_ = probe.Close()
	_ = os.Remove(name)
	if statErr != nil {
		return 0, fmt.Errorf("probing the default file mode in %s: %w", dir, statErr)
	}
	return info.Mode().Perm(), nil
}

// createExclusive creates a new, uniquely named file beside a destination
// called base, in dir, requesting perm -- which the kernel narrows by the
// umask, as for any file. A name collision just means picking another random
// name and trying again, the same way os.CreateTemp retries.
func createExclusive(dir, base, suffix string, perm os.FileMode) (*os.File, error) {
	for attempt := 0; attempt < 10000; attempt++ {
		name := filepath.Join(dir, fmt.Sprintf("%s.%d.%x.%s", base, os.Getpid(), rand.Uint64(), suffix))
		f, err := os.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_EXCL, perm)
		if err == nil {
			return f, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("could not create a unique %s file in %s", suffix, dir)
}

// publishImport publishes the set and translates a context ending into the
// diagnostic the user should see.
//
// It exists as a named function rather than a few lines inside RunE because
// the deadline it handles lands in a window that is not reachable on demand
// from the command: Closure shares the context, so an expired deadline fails
// the walk first and never reaches publication. A test can call this directly
// with an already-expired deadline; a mutation check confirmed that without
// this seam, removing the translation broke no test at all.
func publishImport(ctx context.Context, timeout time.Duration, path string, raw []byte) error {
	if err := publish(ctx, path, raw); err != nil {
		// publish reports a done context as the signal sentinel because it
		// does not know the timeout. This does, so it can say which it was.
		if ctxErr := walkEnded(ctx, timeout, "before publishing to "+path); ctxErr != nil {
			return ctxErr
		}
		return err
	}
	return nil
}

// walkEnded reports why the walk's context ended, or nil if it did not.
//
// A deadline and a signal both cancel the context, but they are not the same
// thing to a user: one is the --timeout they set, the other is the Ctrl-C they
// pressed. Reporting a timeout as "interrupted before completion" sends them
// looking for a signal that never arrived. Both are operational failures, so
// both still exit 2.
// what names the stage that was in progress, so a deadline at the publication
// boundary does not claim the import was still fetching.
func walkEnded(ctx context.Context, timeout time.Duration, what string) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return fmt.Errorf(
			"timed out after %s %s; "+
				"raise --timeout, or pass --timeout 0 to wait indefinitely",
			timeout, what)
	case ctx.Err() != nil:
		return errInterrupted
	}
	return nil
}

// countServices counts the services described by the imported set.
func countServices(set *descriptorpb.FileDescriptorSet) int {
	var n int
	for _, f := range set.GetFile() {
		n += len(f.GetService())
	}
	return n
}

// importSummary is CLI-local metadata, not an admin-contract message, so it
// renders with encoding/json rather than the protojson path writeJSON takes.
type importSummary struct {
	Files    []string `json:"files"`
	Services int      `json:"services"`
	Bytes    int      `json:"bytes"`
	Path     string   `json:"path"`
}

func writeImportSummary(cmd *cobra.Command, asJSON bool, files []string, services, size int, path string) error {
	if asJSON {
		enc := json.NewEncoder(cmd.OutOrStdout())
		enc.SetIndent("", "  ")
		return enc.Encode(importSummary{
			Files: files, Services: services, Bytes: size, Path: path,
		})
	}
	payload := newPayloadWriter(cmd)
	payload.printf("imported %d file(s) from %d service(s) -> %s\n",
		len(files), services, path)
	return payload.err
}
