package stub

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/yinghanhung/simulacra/internal/schema"
)

// LoadDirs loads every *.yaml / *.yml file under the given directories,
// compiles each stub against the registry, and returns all compiled stubs
// plus every error encountered (it does not stop at the first one, so
// `simulacra check` can report everything at once). Files are visited in
// sorted path order for deterministic stub ordering.
func LoadDirs(reg *schema.Registry, dirs []string) ([]*Compiled, []error) {
	var paths []string
	var errs []error
	for _, dir := range dirs {
		err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			if ext := strings.ToLower(filepath.Ext(path)); ext == ".yaml" || ext == ".yml" {
				paths = append(paths, path)
			}
			return nil
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("scanning %s: %w", dir, err))
		}
	}
	sort.Strings(paths)

	var out []*Compiled
	compiler := NewCompiler(reg)
	for _, path := range paths {
		stubs, docs, err := parseFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i, s := range stubs {
			source := fmt.Sprintf("%s#%d", path, i)
			c, err := compiler.Compile(s, source)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			c.ID = source
			c.Document = docs[i]
			out = append(out, c)
		}
	}
	return out, errs
}

// parseFile reads one YAML file holding a list of stubs, decoding every
// `---` document in the file. Parsing is strict: unknown keys are errors,
// so unsupported/future syntax fails loudly instead of being ignored. The
// second return value carries each stub's normalized document (design §3.1),
// harvested from a parallel node pass over the same bytes.
func parseFile(path string) ([]Stub, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var stubs []Stub
	for {
		var doc []Stub
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		stubs = append(stubs, doc...)
	}
	docs, err := SplitDocuments(data)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(docs) != len(stubs) {
		return nil, nil, fmt.Errorf("parsing %s: %d stubs but %d documents; file structure not understood", path, len(stubs), len(docs))
	}
	return stubs, docs, nil
}

// SplitDocuments renders each sequence item of each YAML document as a
// normalized per-stub document, in file order. It is the one splitter for the
// file grammar: parseFile uses it to load a directory, and the CLI's
// `stub add -f` uses it to turn a file into one CreateStub call per stub, so
// files and the admin API can never diverge on what "one stub" means.
//
// Documents are self-contained: aliases are expanded, comments and styling
// stripped. A file that is not a list of stubs is an error.
//
// The struct decoder in parseFile already rejected non-sequence documents, so
// the sequence error here can only fire on shapes it also rejected; the count
// check in parseFile is the belt to that suspender.
func SplitDocuments(data []byte) ([]string, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var docs []string
	for {
		var doc yaml.Node
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return docs, nil
			}
			return nil, err
		}
		if len(doc.Content) == 0 {
			continue
		}
		seq := doc.Content[0]
		if seq.Kind != yaml.SequenceNode {
			return nil, errors.New("stub files hold a list of stubs")
		}
		for _, item := range seq.Content {
			rendered, err := renderNode(item)
			if err != nil {
				return nil, err
			}
			docs = append(docs, rendered)
		}
	}
}
