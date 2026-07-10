package stub

import (
	"bytes"
	"fmt"
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
	for _, path := range paths {
		stubs, err := parseFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for i, s := range stubs {
			source := fmt.Sprintf("%s#%d", path, i)
			c, err := Compile(reg, s, source)
			if err != nil {
				errs = append(errs, err)
				continue
			}
			out = append(out, c)
		}
	}
	return out, errs
}

// parseFile reads one YAML file holding a list of stubs. Parsing is strict:
// unknown keys are errors, so unsupported/future syntax fails loudly.
func parseFile(path string) ([]Stub, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	var stubs []Stub
	if err := dec.Decode(&stubs); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return stubs, nil
}
