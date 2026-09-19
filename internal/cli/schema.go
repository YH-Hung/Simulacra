package cli

import (
	"errors"
	"fmt"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/descriptorpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

func newSchemaCmd() *cobra.Command { return newSchemaCmdWithClient(newAdminClient) }

func newSchemaCmdWithClient(newClient clientFactory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "schema",
		Short: "Inspect and register the descriptors a running server serves",
	}
	cmd.AddCommand(newSchemaRegisterCmd(newClient), newSchemaListCmd(newClient))
	return cmd
}

func newSchemaRegisterCmd(newClient clientFactory) *cobra.Command {
	var files []string
	cmd := &cobra.Command{
		Use:         "register",
		Short:       "Register descriptor sets with a running server",
		Annotations: clientAnnotations(),
		RunE: func(cmd *cobra.Command, _ []string) error {
			if len(files) == 0 {
				return errors.New("--file <descriptor-set> is required")
			}
			merged, err := mergeDescriptorSets(files)
			if err != nil {
				return err
			}
			raw, err := proto.Marshal(merged)
			if err != nil {
				return fmt.Errorf("encoding the merged descriptor set: %w", err)
			}
			client, err := newClient(cmd)
			if err != nil {
				return err
			}
			ctx, stop := signalContext(cmd)
			defer stop()
			callCtx, cancel := client.callContext(ctx)
			defer cancel()

			resp, err := client.Schema.RegisterSchemas(callCtx,
				connect.NewRequest(&adminv1.RegisterSchemasRequest{DescriptorSet: raw}))
			if err != nil {
				return rpcError(ctx, err)
			}
			// Zero newly-registered files is a success: re-registering an
			// unchanged set is a no-op, not a failure.
			payload := newPayloadWriter(cmd)
			payload.printf("registered %d new file(s); %d service(s) available\n",
				len(resp.Msg.GetRegisteredFiles()), resp.Msg.GetServiceCount())
			return payload.err
		},
	}
	clientFlags{}.register(cmd)
	cmd.Flags().StringArrayVarP(&files, "file", "f", nil,
		"serialized FileDescriptorSet to register, e.g. a buf image (repeatable)")
	return cmd
}

func newSchemaListCmd(newClient clientFactory) *cobra.Command {
	out := &outputFlag{}
	cmd := &cobra.Command{
		Use:         "list",
		Short:       "List the services a running server is serving",
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

			resp, err := client.Schema.ListServices(callCtx,
				connect.NewRequest(&adminv1.ListServicesRequest{}))
			if err != nil {
				return rpcError(ctx, err)
			}
			if out.json() {
				return writeJSON(cmd.OutOrStdout(), resp.Msg)
			}
			services := resp.Msg.GetServices()
			if len(services) == 0 {
				cmd.PrintErrln("no services registered")
				return nil
			}
			// The listing is the payload, so it goes to stdout explicitly:
			// cmd.Printf routes through OutOrStderr, which falls back to
			// os.Stderr in the real binary (design §4).
			payload := newPayloadWriter(cmd)
			for _, svc := range services {
				payload.printf("%s (%s)\n", svc.GetName(), svc.GetFile())
				for _, m := range svc.GetMethods() {
					payload.printf("  %s(%s%s) returns (%s%s)\n",
						m.GetName(),
						streamMarker(m.GetClientStreaming()), m.GetInputType(),
						streamMarker(m.GetServerStreaming()), m.GetOutputType())
				}
			}
			return payload.err
		},
	}
	clientFlags{}.register(cmd)
	out.register(cmd)
	return cmd
}

func streamMarker(streaming bool) string {
	if streaming {
		return "stream "
	}
	return ""
}

// mergedFile records where a file path was first seen, so a conflict can name
// both sources.
type mergedFile struct {
	path string
	file *descriptorpb.FileDescriptorProto
}

// mergeDescriptorSets reads each path as a FileDescriptorSet and merges them
// into one, keeping the first occurrence of each file path.
//
// One merged call, because RegisterSchemas is all-or-nothing and sequential
// per-file calls would forfeit that.
//
// Deduping is not an optimization. Registering a set that names a path twice
// fails outright ("file appears multiple times"), and duplicates are the
// common case: any two sets built with --include_imports or `buf build -o`
// both embed the well-known types they use. A later file carrying the same
// path with different contents is an error rather than a silent pick, because
// keeping one of two conflicting definitions would register a schema the user
// never described.
func mergeDescriptorSets(paths []string) (*descriptorpb.FileDescriptorSet, error) {
	merged := &descriptorpb.FileDescriptorSet{}
	seen := map[string]mergedFile{}
	for _, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", path, err)
		}
		var set descriptorpb.FileDescriptorSet
		if err := proto.Unmarshal(raw, &set); err != nil {
			return nil, fmt.Errorf("parsing %s as a FileDescriptorSet: %w", path, err)
		}
		for _, file := range set.GetFile() {
			name := file.GetName()
			if prior, ok := seen[name]; ok {
				if !proto.Equal(prior.file, file) {
					return nil, fmt.Errorf(
						"%s and %s both define %s with different contents; "+
							"register them separately, or rebuild both from one source",
						prior.path, path, name)
				}
				continue
			}
			seen[name] = mergedFile{path: path, file: file}
			merged.File = append(merged.File, file)
		}
	}
	return merged, nil
}
