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
		stubs, err := parseFile(path)
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
			out = append(out, c)
		}
	}
	return out, errs
}

// parseFile reads one YAML file holding a list of stubs, decoding every
// `---` document in the file. Parsing is strict: unknown keys are errors,
// so unsupported/future syntax fails loudly instead of being ignored.
func parseFile(path string) ([]Stub, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var stubs []Stub
	for {
		var doc []Stub
		if err := dec.Decode(&doc); err != nil {
			if errors.Is(err, io.EOF) {
				return stubs, nil
			}
			return nil, fmt.Errorf("parsing %s: %w", path, err)
		}
		stubs = append(stubs, doc...)
	}
}
