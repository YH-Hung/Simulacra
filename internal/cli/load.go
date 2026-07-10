package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// sources holds the schema/stub flags shared by `serve` and `check`.
type sources struct {
	protoDirs      []string
	descriptorSets []string
	stubDirs       []string
}

func (s *sources) register(cmd *cobra.Command) {
	cmd.Flags().StringArrayVar(&s.protoDirs, "proto", nil, "directory of .proto files to compile (repeatable; the directory is the import root)")
	cmd.Flags().StringArrayVar(&s.descriptorSets, "descriptors", nil, "serialized FileDescriptorSet file, e.g. a buf image (repeatable)")
	cmd.Flags().StringArrayVar(&s.stubDirs, "stubs", nil, "directory of stub YAML files (repeatable)")
}

func (s *sources) buildRegistry(ctx context.Context) (*schema.Registry, error) {
	if len(s.protoDirs) == 0 && len(s.descriptorSets) == 0 {
		return nil, errors.New("at least one schema source is required: --proto <dir> or --descriptors <file>")
	}
	reg := schema.NewRegistry()
	for _, d := range s.protoDirs {
		if err := reg.AddProtoDir(ctx, d); err != nil {
			return nil, err
		}
	}
	for _, f := range s.descriptorSets {
		if err := reg.AddDescriptorSetFile(f); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

// loadStubs loads and compiles stubs, printing every error to the command's
// error stream. Returns an error if any stub failed.
func (s *sources) loadStubs(cmd *cobra.Command, reg *schema.Registry) ([]*stub.Compiled, error) {
	stubs, errs := stub.LoadDirs(reg, s.stubDirs)
	for _, e := range errs {
		cmd.PrintErrln("stub error:", e)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%d invalid stub(s)", len(errs))
	}
	return stubs, nil
}
