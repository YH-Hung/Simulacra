package cli

import (
	"fmt"
	"io"
	"text/tabwriter"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
)

const (
	outputText = "text"
	outputJSON = "json"
)

// outputFlag is the format flag read commands carry. It has no shorthand: -o
// is stub export's --out, and binding it here would collide on the one command
// that takes both.
type outputFlag struct {
	format string
}

func (f *outputFlag) register(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.format, "output", outputText,
		`output format: "text" or "json"`)
}

func (f *outputFlag) validate() error {
	switch f.format {
	case outputText, outputJSON:
		return nil
	}
	return fmt.Errorf("--output must be %q or %q (got %q)", outputText, outputJSON, f.format)
}

func (f *outputFlag) json() bool { return f.format == outputJSON }

// jsonOptions renders a response as the frozen contract shape.
//
// UseProtoNames matches the names matcher paths and the stub grammar use.
// EmitDefaultValues keeps scripts total: without it a false `passed` is absent
// and `jq .passed` reads null. EmitUnpopulated is deliberately not used — it
// would also emit null for unset message fields.
//
// Note for tests: protojson output is not byte-stable. Decode it and assert on
// fields; never compare it as a string.
var jsonOptions = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
	Multiline:         true,
	Indent:            "  ",
}

// streamJSONOptions is jsonOptions for `calls tail`: a stream has no enclosing
// document, so each call is one compact object on its own line.
var streamJSONOptions = protojson.MarshalOptions{
	UseProtoNames:     true,
	EmitDefaultValues: true,
}

func writeJSON(w io.Writer, msg proto.Message) error {
	raw, err := jsonOptions.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

func writeJSONLine(w io.Writer, msg proto.Message) error {
	raw, err := streamJSONOptions.Marshal(msg)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", raw)
	return err
}

// newTable returns a tabwriter over w. Callers write tab-separated rows and
// Flush.
func newTable(w io.Writer) *tabwriter.Writer {
	return tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
}

// payloadWriter writes a command's payload to stdout and keeps the first write
// error, so a caller checks once before returning instead of at every line.
//
// Checking at all is the point. A payload write that fails is the whole
// outcome of the command: `simulacra schema list > services.txt` against a
// full disk produced no listing, and exiting 0 would tell a script it has one.
// The JSON paths already report this by returning writeJSON's error; this is
// how the text paths say the same thing rather than failing silently.
//
// Commentary is deliberately left out of it. cmd.PrintErr* keeps ignoring its
// errors, because a warning that could not be printed must not replace the
// real outcome with a diagnostic about the warning.
type payloadWriter struct {
	w   io.Writer
	err error
}

func newPayloadWriter(cmd *cobra.Command) *payloadWriter {
	return &payloadWriter{w: cmd.OutOrStdout()}
}

// printf writes one piece of payload, and does nothing once a write has
// failed: the first error is the one that describes what broke, and later
// writes to a broken writer only restate it.
func (p *payloadWriter) printf(format string, args ...any) {
	if p.err != nil {
		return
	}
	_, p.err = fmt.Fprintf(p.w, format, args...)
}
