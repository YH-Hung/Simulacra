# Simulacra M1 — Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A single Go binary where `simulacra serve --proto ./protos --stubs ./stubs` answers a real grpcurl unary call correctly — dynamic schema loading, YAML stubs, structured matching, server reflection, and health, with no code generation anywhere.

**Architecture:** One gRPC server whose only handler is grpc-go's `UnknownServiceHandler`; every request is decoded into a `dynamicpb` message against a schema registry built at startup (runtime-compiled `.proto` trees via `bufbuild/protocompile`, or descriptor-set files), matched against compiled YAML stubs, and answered with a pre-built dynamic response message. Reflection and health are served so standard tooling works out of the box.

**Tech Stack:** Go 1.25, `google.golang.org/grpc`, `google.golang.org/protobuf` (`dynamicpb`, `protoreflect`, `protojson`), `github.com/bufbuild/protocompile`, `gopkg.in/yaml.v3`, `github.com/spf13/cobra`. Tests use the standard library only.

**Reference:** `PROPOSAL.md` §4 (architecture), §5 (stub model — M1 implements the structured-matcher subset), §6 (schema management — proto dirs + descriptor sets only in M1), §12 (roadmap M1 row).

**M1 scope guard (YAGNI):** unary only; structured matchers only (no CEL); `respond.message` only (no delays, no status responses, no templating, no metadata/trailers on responses); no admin API, journal, dashboard, or hot reload. Stub files are parsed **strictly** — unknown YAML keys are load errors, so M2 syntax fails loudly instead of being silently ignored.

---

## File structure

```
simulacra/
├── go.mod                                  # module github.com/yinghanhung/simulacra (rename before publishing)
├── .gitignore
├── LICENSE                                 # Apache-2.0
├── cmd/simulacra/main.go                   # entry point → internal/cli
├── internal/
│   ├── cli/
│   │   ├── root.go                         # cobra root command
│   │   ├── load.go                         # shared flag→registry/stubs loading
│   │   ├── serve.go                        # `simulacra serve`
│   │   └── check.go                        # `simulacra check`
│   ├── schema/
│   │   ├── registry.go                     # Registry: proto-dir compile, descriptor sets, lookups
│   │   └── registry_test.go
│   ├── match/
│   │   ├── match.go                        # Block/Rules YAML types, Compile, Eval
│   │   └── match_test.go
│   ├── stub/
│   │   ├── stub.go                         # Stub YAML types, Compile (per-stub), BuildMessage
│   │   ├── loader.go                       # LoadDirs: walk dirs, strict parse, compile, aggregate errors
│   │   ├── store.go                        # Store: priority/times selection
│   │   └── stub_test.go, loader_test.go, store_test.go
│   └── dataplane/
│       ├── server.go                       # grpc server, unknown handler, reflection, health
│       └── server_test.go                  # in-process e2e with a dynamic client
├── testdata/protos/shop/v1/order.proto     # fixture schema used by every test
└── .github/workflows/ci.yml
```

Dependency direction: `cli → dataplane → stub → match → schema`. `schema` depends only on protobuf libraries. No package imports `cli`.

---

### Task 1: Repository scaffolding

**Files:**
- Create: `go.mod`, `.gitignore`, `LICENSE`, `cmd/simulacra/main.go` (placeholder that compiles)

- [ ] **Step 1: Initialize the module and directories**

```bash
cd /Users/yinghanhung/Projects/Polyglot_gRPC/Simulacra
go mod init github.com/yinghanhung/simulacra
mkdir -p cmd/simulacra internal/cli internal/schema internal/match internal/stub internal/dataplane testdata/protos/shop/v1 .github/workflows
```

- [ ] **Step 2: Write `.gitignore`**

```gitignore
/simulacra
/dist/
*.test
```

- [ ] **Step 3: Add the Apache-2.0 license**

```bash
curl -fsSL -o LICENSE https://www.apache.org/licenses/LICENSE-2.0.txt
```

- [ ] **Step 4: Write a minimal `cmd/simulacra/main.go` so the module builds**

```go
package main

func main() {}
```

- [ ] **Step 5: Verify the module builds**

Run: `go build ./...`
Expected: exits 0, no output.

- [ ] **Step 6: Commit**

```bash
git add go.mod .gitignore LICENSE cmd/
git commit -m "feat: scaffold Go module for simulacra core"
```

---

### Task 2: Schema registry — compile `.proto` directories at runtime

**Files:**
- Create: `testdata/protos/shop/v1/order.proto`
- Create: `internal/schema/registry.go`
- Test: `internal/schema/registry_test.go`

- [ ] **Step 1: Write the fixture proto (used by all subsequent tasks)**

`testdata/protos/shop/v1/order.proto`:

```proto
syntax = "proto3";

package shop.v1;

import "google/protobuf/timestamp.proto";

enum Region {
  REGION_UNSPECIFIED = 0;
  EU = 1;
  UK = 2;
  US = 3;
}

enum OrderStatus {
  ORDER_STATUS_UNSPECIFIED = 0;
  ORDER_STATUS_PENDING = 1;
  ORDER_STATUS_SHIPPED = 2;
}

message Customer {
  string id = 1;
  Region region = 2;
}

message Item {
  string sku = 1;
  int64 qty = 2;
}

message GetOrderRequest {
  string order_id = 1;
  Customer customer = 2;
  repeated Item items = 3;
  repeated string tags = 4;
}

message GetOrderResponse {
  string order_id = 1;
  OrderStatus status = 2;
  string note = 3;
  google.protobuf.Timestamp eta = 4;
}

service OrderService {
  rpc GetOrder(GetOrderRequest) returns (GetOrderResponse);
  rpc WatchOrder(GetOrderRequest) returns (stream GetOrderResponse);
}
```

The `google/protobuf/timestamp.proto` import deliberately exercises well-known-type resolution; `WatchOrder` exists so tests can prove streaming methods are rejected cleanly in M1.

- [ ] **Step 2: Write the failing test**

`internal/schema/registry_test.go`:

```go
package schema

import (
	"context"
	"testing"
)

const testProtoDir = "../../testdata/protos"

func TestAddProtoDirAndLookupMethod(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}

	// Both slash forms must resolve (grpc-go hands us "/pkg.Svc/Method").
	for _, name := range []string{"/shop.v1.OrderService/GetOrder", "shop.v1.OrderService/GetOrder"} {
		m, err := reg.LookupMethod(name)
		if err != nil {
			t.Fatalf("LookupMethod(%q): %v", name, err)
		}
		if got := string(m.Input().FullName()); got != "shop.v1.GetOrderRequest" {
			t.Errorf("input type = %s, want shop.v1.GetOrderRequest", got)
		}
		if got := string(m.Output().FullName()); got != "shop.v1.GetOrderResponse" {
			t.Errorf("output type = %s, want shop.v1.GetOrderResponse", got)
		}
	}

	if _, err := reg.LookupMethod("/shop.v1.OrderService/NoSuchMethod"); err == nil {
		t.Error("expected error for unknown method")
	}
	if _, err := reg.LookupMethod("/no.such.Service/GetOrder"); err == nil {
		t.Error("expected error for unknown service")
	}
	if _, err := reg.LookupMethod("garbage"); err == nil {
		t.Error("expected error for malformed method name")
	}

	svcs := reg.Services()
	found := false
	for _, s := range svcs {
		if string(s.FullName()) == "shop.v1.OrderService" {
			found = true
		}
	}
	if !found {
		t.Errorf("Services() = %v, want to contain shop.v1.OrderService", svcs)
	}
}

func TestAddProtoDirEmpty(t *testing.T) {
	reg := NewRegistry()
	if err := reg.AddProtoDir(context.Background(), t.TempDir()); err == nil {
		t.Error("expected error for directory with no .proto files")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/schema/ -v`
Expected: FAIL — `undefined: NewRegistry` (package doesn't compile yet).

- [ ] **Step 4: Implement the registry**

`internal/schema/registry.go`:

```go
// Package schema builds and queries a registry of protobuf descriptors from
// runtime-compiled .proto trees and descriptor-set files. It is the single
// source of truth for "what services and message shapes does the mock know".
package schema

import (
	"context"
	"fmt"
	"io/fs"
	"path/filepath"
	"strings"

	"github.com/bufbuild/protocompile"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
)

type Registry struct {
	files *protoregistry.Files
}

func NewRegistry() *Registry {
	return &Registry{files: new(protoregistry.Files)}
}

// Files exposes the registry as a protodesc.Resolver (used by grpc reflection).
func (r *Registry) Files() *protoregistry.Files { return r.files }

// AddProtoDir compiles every .proto file found under root, treating root as
// the single import path (imports inside the files are resolved relative to
// root; well-known types are provided automatically).
func (r *Registry) AddProtoDir(ctx context.Context, root string) error {
	var names []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(path, ".proto") {
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			names = append(names, filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("scanning %s: %w", root, err)
	}
	if len(names) == 0 {
		return fmt.Errorf("no .proto files found under %s", root)
	}

	compiler := protocompile.Compiler{
		Resolver: protocompile.WithStandardImports(
			&protocompile.SourceResolver{ImportPaths: []string{root}},
		),
	}
	compiled, err := compiler.Compile(ctx, names...)
	if err != nil {
		return fmt.Errorf("compiling protos under %s: %w", root, err)
	}
	for _, fd := range compiled {
		if err := r.AddFile(fd); err != nil {
			return err
		}
	}
	return nil
}

// AddFile registers a file descriptor and, first, all of its imports.
// A path that is already registered is skipped (first registration wins).
func (r *Registry) AddFile(fd protoreflect.FileDescriptor) error {
	if _, err := r.files.FindFileByPath(fd.Path()); err == nil {
		return nil
	}
	imps := fd.Imports()
	for i := 0; i < imps.Len(); i++ {
		if err := r.AddFile(imps.Get(i).FileDescriptor); err != nil {
			return err
		}
	}
	return r.files.RegisterFile(fd)
}

// LookupMethod resolves "pkg.Service/Method" or "/pkg.Service/Method".
func (r *Registry) LookupMethod(fullMethod string) (protoreflect.MethodDescriptor, error) {
	name := strings.TrimPrefix(fullMethod, "/")
	idx := strings.LastIndex(name, "/")
	if idx <= 0 || idx == len(name)-1 {
		return nil, fmt.Errorf("invalid method name %q (want package.Service/Method)", fullMethod)
	}
	svcName, methodName := name[:idx], name[idx+1:]
	d, err := r.files.FindDescriptorByName(protoreflect.FullName(svcName))
	if err != nil {
		return nil, fmt.Errorf("service %q is not registered (no schema source declares it)", svcName)
	}
	svc, ok := d.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, fmt.Errorf("%q is not a service", svcName)
	}
	m := svc.Methods().ByName(protoreflect.Name(methodName))
	if m == nil {
		return nil, fmt.Errorf("method %q not found on service %q", methodName, svcName)
	}
	return m, nil
}

// Services returns every service descriptor across all registered files.
func (r *Registry) Services() []protoreflect.ServiceDescriptor {
	var out []protoreflect.ServiceDescriptor
	r.files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		svcs := fd.Services()
		for i := 0; i < svcs.Len(); i++ {
			out = append(out, svcs.Get(i))
		}
		return true
	})
	return out
}
```

- [ ] **Step 5: Fetch dependencies and run the test**

```bash
go get github.com/bufbuild/protocompile@latest google.golang.org/protobuf@latest
go test ./internal/schema/ -v
```

Expected: PASS (both tests).

- [ ] **Step 6: Commit**

```bash
git add testdata/ internal/schema/ go.mod go.sum
git commit -m "feat: schema registry with runtime .proto compilation"
```

---

### Task 3: Schema registry — descriptor-set files

**Files:**
- Modify: `internal/schema/registry.go` (add `AddDescriptorSetFile`)
- Test: `internal/schema/registry_test.go` (append)

- [ ] **Step 1: Write the failing test**

Append to `internal/schema/registry_test.go`. The test builds a self-contained `FileDescriptorSet` from the already-compiled fixture (no binary file checked in). Add to the existing import block: `"os"`, `"path/filepath"`, `"google.golang.org/protobuf/proto"`, `"google.golang.org/protobuf/reflect/protodesc"`, `"google.golang.org/protobuf/reflect/protoreflect"`, `"google.golang.org/protobuf/types/descriptorpb"`.

```go

// buildDescriptorSet marshals fd and its transitive imports, deps first —
// the same self-contained shape `buf build` and `protoc --include_imports` emit.
func buildDescriptorSet(t *testing.T, fd protoreflect.FileDescriptor) []byte {
	t.Helper()
	seen := map[string]bool{}
	set := new(descriptorpb.FileDescriptorSet)
	var add func(protoreflect.FileDescriptor)
	add = func(f protoreflect.FileDescriptor) {
		if seen[f.Path()] {
			return
		}
		seen[f.Path()] = true
		imps := f.Imports()
		for i := 0; i < imps.Len(); i++ {
			add(imps.Get(i).FileDescriptor)
		}
		set.File = append(set.File, protodesc.ToFileDescriptorProto(f))
	}
	add(fd)
	data, err := proto.Marshal(set)
	if err != nil {
		t.Fatalf("marshaling descriptor set: %v", err)
	}
	return data
}

func TestAddDescriptorSetFile(t *testing.T) {
	src := NewRegistry()
	if err := src.AddProtoDir(context.Background(), testProtoDir); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	m, err := src.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	setPath := filepath.Join(t.TempDir(), "shop.binpb")
	if err := os.WriteFile(setPath, buildDescriptorSet(t, m.ParentFile()), 0o644); err != nil {
		t.Fatalf("writing descriptor set: %v", err)
	}

	reg := NewRegistry()
	if err := reg.AddDescriptorSetFile(setPath); err != nil {
		t.Fatalf("AddDescriptorSetFile: %v", err)
	}
	if _, err := reg.LookupMethod("shop.v1.OrderService/GetOrder"); err != nil {
		t.Errorf("LookupMethod after descriptor-set load: %v", err)
	}
}

func TestAddDescriptorSetFileRejectsGarbage(t *testing.T) {
	p := filepath.Join(t.TempDir(), "garbage.binpb")
	if err := os.WriteFile(p, []byte("not a descriptor set"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := NewRegistry().AddDescriptorSetFile(p); err == nil {
		t.Error("expected error for non-descriptor-set file")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/schema/ -v`
Expected: FAIL — `undefined: (*Registry).AddDescriptorSetFile` (compile error).

- [ ] **Step 3: Implement `AddDescriptorSetFile`**

Append to `internal/schema/registry.go` (add `os`, `google.golang.org/protobuf/proto`, `google.golang.org/protobuf/reflect/protodesc`, `google.golang.org/protobuf/types/descriptorpb` to imports):

```go
// AddDescriptorSetFile loads a serialized FileDescriptorSet (e.g. a buf image
// or `protoc --descriptor_set_out --include_imports` output). The set must be
// self-contained: every import must be included in the set.
func (r *Registry) AddDescriptorSetFile(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("reading descriptor set: %w", err)
	}
	set := new(descriptorpb.FileDescriptorSet)
	if err := proto.Unmarshal(data, set); err != nil {
		return fmt.Errorf("%s is not a valid FileDescriptorSet: %w", path, err)
	}
	if len(set.File) == 0 {
		return fmt.Errorf("%s is not a valid FileDescriptorSet: contains no files", path)
	}
	files, err := protodesc.NewFiles(set)
	if err != nil {
		return fmt.Errorf("loading %s (descriptor sets must be self-contained; build with `buf build -o` or `protoc --include_imports`): %w", path, err)
	}
	var regErr error
	files.RangeFiles(func(fd protoreflect.FileDescriptor) bool {
		regErr = r.AddFile(fd)
		return regErr == nil
	})
	return regErr
}
```

Note: `proto.Unmarshal` accepts some garbage inputs as empty messages, which is why the empty-set check exists.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/schema/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/schema/
git commit -m "feat: load descriptor-set files into schema registry"
```

---

### Task 4: Structured matcher engine

**Files:**
- Create: `internal/match/match.go`
- Test: `internal/match/match_test.go`

Semantics being implemented (from PROPOSAL.md §5, structured subset):

- `match` block has `metadata` (map: key → rules) and `message` (map: dotted field path → rules).
- Ops: `eq`, `ne`, `in`, `matches` (regex, string fields only), `present`, `contains` (repeated fields).
- Message paths traverse **singular message fields** only; every path and literal is validated against the descriptor at compile time (this is what makes `simulacra check` useful).
- Enums accept value **names** (`EU`) or numbers; unknown names are compile errors.
- Unset fields read as proto defaults for `eq`/`ne`/`in`; `present` uses real presence (`Has`): for implicit-presence proto3 scalars that means "non-default", for messages/optionals it means "set", for repeated it means "non-empty".
- Metadata rules: a rule passes if **any** value for that key satisfies it (gRPC metadata is multi-valued); `ne` passes only if **no** value equals.
- A `nil`/empty block matches everything. All rules are AND-ed.

- [ ] **Step 1: Write the failing test**

`internal/match/match_test.go`:

```go
package match

import (
	"context"
	"testing"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/schema"
)

func requestDesc(t *testing.T) protoreflect.MessageDescriptor {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	m, err := reg.LookupMethod("shop.v1.OrderService/GetOrder")
	if err != nil {
		t.Fatalf("LookupMethod: %v", err)
	}
	return m.Input()
}

func msg(t *testing.T, desc protoreflect.MessageDescriptor, jsonBody string) protoreflect.Message {
	t.Helper()
	m := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal([]byte(jsonBody), m); err != nil {
		t.Fatalf("building test message from %s: %v", jsonBody, err)
	}
	return m
}

func TestEval(t *testing.T) {
	desc := requestDesc(t)
	req := `{
		"order_id": "o-123",
		"customer": {"id": "c-9", "region": "EU"},
		"items": [{"sku": "A1", "qty": "3"}, {"sku": "B2", "qty": "1"}],
		"tags": ["prio", "gift"]
	}`

	cases := []struct {
		name  string
		block *Block
		body  string
		md    metadata.MD
		want  bool
	}{
		{"nil block matches all", nil, req, nil, true},
		{"eq string", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}}}, req, nil, true},
		{"eq string miss", &Block{Message: map[string]Rules{"order_id": {"eq": "other"}}}, req, nil, false},
		{"ne", &Block{Message: map[string]Rules{"order_id": {"ne": "other"}}}, req, nil, true},
		{"nested path", &Block{Message: map[string]Rules{"customer.id": {"eq": "c-9"}}}, req, nil, true},
		{"enum by name", &Block{Message: map[string]Rules{"customer.region": {"eq": "EU"}}}, req, nil, true},
		{"enum in list", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"EU", "UK"}}}}, req, nil, true},
		{"enum in list miss", &Block{Message: map[string]Rules{"customer.region": {"in": []any{"UK", "US"}}}}, req, nil, false},
		{"regex", &Block{Message: map[string]Rules{"order_id": {"matches": "^o-\\d+$"}}}, req, nil, true},
		{"present true", &Block{Message: map[string]Rules{"customer": {"present": true}}}, req, nil, true},
		{"present false on unset", &Block{Message: map[string]Rules{"customer": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"present on repeated means non-empty", &Block{Message: map[string]Rules{"items": {"present": true}}}, `{"order_id":"x"}`, nil, false},
		{"contains on repeated string", &Block{Message: map[string]Rules{"tags": {"contains": "gift"}}}, req, nil, true},
		{"contains miss", &Block{Message: map[string]Rules{"tags": {"contains": "bulk"}}}, req, nil, false},
		{"unset leaf reads as default", &Block{Message: map[string]Rules{"customer.id": {"eq": ""}}}, `{"order_id":"x"}`, nil, true},
		{"two rules AND", &Block{Message: map[string]Rules{"order_id": {"eq": "o-123"}, "customer.id": {"eq": "nope"}}}, req, nil, false},
		{"metadata eq", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
		{"metadata eq missing key", &Block{Metadata: map[string]Rules{"x-tenant": {"eq": "acme"}}}, req, nil, false},
		{"metadata present", &Block{Metadata: map[string]Rules{"authorization": {"present": true}}}, req, metadata.Pairs("authorization", "Bearer x"), true},
		{"metadata any-value semantics", &Block{Metadata: map[string]Rules{"x-tag": {"eq": "b"}}}, req, metadata.Pairs("x-tag", "a", "x-tag", "b"), true},
		{"metadata regex", &Block{Metadata: map[string]Rules{"x-tenant": {"matches": "^ac"}}}, req, metadata.Pairs("x-tenant", "acme"), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Compile(desc, tc.block)
			if err != nil {
				t.Fatalf("Compile: %v", err)
			}
			if got := c.Eval(msg(t, desc, tc.body), tc.md); got != tc.want {
				t.Errorf("Eval = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCompileErrors(t *testing.T) {
	desc := requestDesc(t)
	cases := []struct {
		name  string
		block *Block
	}{
		{"unknown field", &Block{Message: map[string]Rules{"no_such_field": {"eq": "x"}}}},
		{"unknown nested field", &Block{Message: map[string]Rules{"customer.nope": {"eq": "x"}}}},
		{"descend through scalar", &Block{Message: map[string]Rules{"order_id.sub": {"eq": "x"}}}},
		{"descend through repeated", &Block{Message: map[string]Rules{"items.sku": {"eq": "x"}}}},
		{"unknown op", &Block{Message: map[string]Rules{"order_id": {"equalz": "x"}}}},
		{"regex on non-string", &Block{Message: map[string]Rules{"customer.region": {"matches": "E.*"}}}},
		{"bad regex", &Block{Message: map[string]Rules{"order_id": {"matches": "("}}}},
		{"contains on singular", &Block{Message: map[string]Rules{"order_id": {"contains": "x"}}}},
		{"eq on repeated", &Block{Message: map[string]Rules{"tags": {"eq": "x"}}}},
		{"unknown enum name", &Block{Message: map[string]Rules{"customer.region": {"eq": "MARS"}}}},
		{"type mismatch", &Block{Message: map[string]Rules{"order_id": {"eq": 42}}}},
		{"metadata unknown op", &Block{Metadata: map[string]Rules{"k": {"has": true}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Compile(desc, tc.block); err == nil {
				t.Error("expected compile error, got nil")
			}
		})
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/match/ -v`
Expected: FAIL — package does not compile (`undefined: Block`, `Compile`).

- [ ] **Step 3: Implement the matcher**

`internal/match/match.go`:

```go
// Package match compiles the structured `match:` block of a stub against a
// message descriptor and evaluates decoded requests against it. Compilation
// validates every field path, operator, and literal, so bad stubs fail at
// load time — never silently at request time.
package match

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Block is the YAML shape of a stub's `match:` section (M1 structured subset).
type Block struct {
	Metadata map[string]Rules `yaml:"metadata"`
	Message  map[string]Rules `yaml:"message"`
}

// Rules maps an operator name (eq, ne, in, matches, present, contains) to its literal.
type Rules map[string]any

type Compiled struct {
	metadata []mdRule
	message  []msgRule
}

type mdRule struct {
	key  string
	op   string
	str  string
	list []string
	re   *regexp.Regexp
	want bool
}

type msgRule struct {
	path []protoreflect.FieldDescriptor
	op   string
	lit  literal
	list []literal
	re   *regexp.Regexp
	want bool
}

// literal is a stub-file literal pre-converted to the leaf field's kind.
type literal struct {
	kind protoreflect.Kind
	str  string
	b    bool
	i    int64
	u    uint64
	f    float64
	enum protoreflect.EnumNumber
}

// Compile validates the block against the request descriptor. A nil block
// compiles to a matcher that accepts everything.
func Compile(input protoreflect.MessageDescriptor, b *Block) (*Compiled, error) {
	c := &Compiled{}
	if b == nil {
		return c, nil
	}
	for key, rules := range b.Metadata {
		for op, raw := range rules {
			r, err := compileMDRule(strings.ToLower(key), op, raw)
			if err != nil {
				return nil, fmt.Errorf("metadata %q: %w", key, err)
			}
			c.metadata = append(c.metadata, r)
		}
	}
	for path, rules := range b.Message {
		fds, err := resolvePath(input, path)
		if err != nil {
			return nil, err
		}
		for op, raw := range rules {
			r, err := compileMsgRule(fds, op, raw)
			if err != nil {
				return nil, fmt.Errorf("message field %q: %w", path, err)
			}
			c.message = append(c.message, r)
		}
	}
	return c, nil
}

func (c *Compiled) Eval(msg protoreflect.Message, md metadata.MD) bool {
	for _, r := range c.metadata {
		if !r.eval(md) {
			return false
		}
	}
	for _, r := range c.message {
		if !r.eval(msg) {
			return false
		}
	}
	return true
}

// --- metadata rules ---

func compileMDRule(key, op string, raw any) (mdRule, error) {
	r := mdRule{key: key, op: op}
	switch op {
	case "eq", "ne", "matches":
		s, ok := raw.(string)
		if !ok {
			return r, fmt.Errorf("%s wants a string, got %T", op, raw)
		}
		if op == "matches" {
			re, err := regexp.Compile(s)
			if err != nil {
				return r, fmt.Errorf("invalid regex: %w", err)
			}
			r.re = re
		} else {
			r.str = s
		}
	case "in":
		items, ok := raw.([]any)
		if !ok {
			return r, fmt.Errorf("in wants a list, got %T", raw)
		}
		for _, it := range items {
			s, ok := it.(string)
			if !ok {
				return r, fmt.Errorf("in wants strings, got %T", it)
			}
			r.list = append(r.list, s)
		}
	case "present":
		want, ok := raw.(bool)
		if !ok {
			return r, fmt.Errorf("present wants true/false, got %T", raw)
		}
		r.want = want
	default:
		return r, fmt.Errorf("unknown metadata operator %q (want eq, ne, in, matches, present)", op)
	}
	return r, nil
}

func (r mdRule) eval(md metadata.MD) bool {
	vals := md.Get(r.key)
	switch r.op {
	case "present":
		return (len(vals) > 0) == r.want
	case "eq":
		return slices.Contains(vals, r.str)
	case "ne":
		return !slices.Contains(vals, r.str)
	case "in":
		for _, v := range vals {
			if slices.Contains(r.list, v) {
				return true
			}
		}
		return false
	case "matches":
		for _, v := range vals {
			if r.re.MatchString(v) {
				return true
			}
		}
		return false
	}
	return false
}

// --- message rules ---

func resolvePath(md protoreflect.MessageDescriptor, path string) ([]protoreflect.FieldDescriptor, error) {
	parts := strings.Split(path, ".")
	out := make([]protoreflect.FieldDescriptor, 0, len(parts))
	cur := md
	for i, p := range parts {
		fd := cur.Fields().ByName(protoreflect.Name(p))
		if fd == nil {
			return nil, fmt.Errorf("field %q not found in %s (path %q)", p, cur.FullName(), path)
		}
		out = append(out, fd)
		if i < len(parts)-1 {
			if fd.Kind() != protoreflect.MessageKind || fd.IsList() || fd.IsMap() {
				return nil, fmt.Errorf("cannot descend into %q in path %q: not a singular message field", p, path)
			}
			cur = fd.Message()
		}
	}
	return out, nil
}

func compileMsgRule(path []protoreflect.FieldDescriptor, op string, raw any) (msgRule, error) {
	leaf := path[len(path)-1]
	r := msgRule{path: path, op: op}
	switch op {
	case "present":
		want, ok := raw.(bool)
		if !ok {
			return r, fmt.Errorf("present wants true/false, got %T", raw)
		}
		r.want = want
		return r, nil
	case "matches":
		if leaf.Kind() != protoreflect.StringKind || leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("matches requires a singular string field, %q is %s", leaf.Name(), leaf.Kind())
		}
		s, ok := raw.(string)
		if !ok {
			return r, fmt.Errorf("matches wants a string regex, got %T", raw)
		}
		re, err := regexp.Compile(s)
		if err != nil {
			return r, fmt.Errorf("invalid regex: %w", err)
		}
		r.re = re
		return r, nil
	case "contains":
		if !leaf.IsList() {
			return r, fmt.Errorf("contains requires a repeated field, %q is singular", leaf.Name())
		}
		lit, err := literalFor(leaf, raw)
		if err != nil {
			return r, err
		}
		r.lit = lit
		return r, nil
	case "eq", "ne":
		if leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("%s requires a singular field, %q is repeated/map (use contains)", op, leaf.Name())
		}
		lit, err := literalFor(leaf, raw)
		if err != nil {
			return r, err
		}
		r.lit = lit
		return r, nil
	case "in":
		if leaf.IsList() || leaf.IsMap() {
			return r, fmt.Errorf("in requires a singular field, %q is repeated/map", leaf.Name())
		}
		items, ok := raw.([]any)
		if !ok {
			return r, fmt.Errorf("in wants a list, got %T", raw)
		}
		for _, it := range items {
			lit, err := literalFor(leaf, it)
			if err != nil {
				return r, err
			}
			r.list = append(r.list, lit)
		}
		return r, nil
	default:
		return r, fmt.Errorf("unknown operator %q (want eq, ne, in, matches, present, contains)", op)
	}
}

func literalFor(fd protoreflect.FieldDescriptor, raw any) (literal, error) {
	k := fd.Kind()
	l := literal{kind: k}
	switch k {
	case protoreflect.StringKind:
		s, ok := raw.(string)
		if !ok {
			return l, fmt.Errorf("field %q is a string, got %T literal", fd.Name(), raw)
		}
		l.str = s
	case protoreflect.BoolKind:
		b, ok := raw.(bool)
		if !ok {
			return l, fmt.Errorf("field %q is a bool, got %T literal", fd.Name(), raw)
		}
		l.b = b
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		i, ok := toInt64(raw)
		if !ok {
			return l, fmt.Errorf("field %q is an integer, got %T literal", fd.Name(), raw)
		}
		l.i = i
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		i, ok := toInt64(raw)
		if !ok || i < 0 {
			return l, fmt.Errorf("field %q is an unsigned integer, got %v", fd.Name(), raw)
		}
		l.u = uint64(i)
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		switch v := raw.(type) {
		case float64:
			l.f = v
		case int:
			l.f = float64(v)
		default:
			return l, fmt.Errorf("field %q is a float, got %T literal", fd.Name(), raw)
		}
	case protoreflect.EnumKind:
		switch v := raw.(type) {
		case string:
			ev := fd.Enum().Values().ByName(protoreflect.Name(v))
			if ev == nil {
				return l, fmt.Errorf("enum %s has no value named %q", fd.Enum().FullName(), v)
			}
			l.enum = ev.Number()
		case int:
			l.enum = protoreflect.EnumNumber(v)
		default:
			return l, fmt.Errorf("field %q is an enum, got %T literal", fd.Name(), raw)
		}
	default:
		return l, fmt.Errorf("field %q has kind %s, which structured matchers do not support yet (bytes/message/map matching arrives with CEL in M2)", fd.Name(), k)
	}
	return l, nil
}

func toInt64(raw any) (int64, bool) {
	switch v := raw.(type) {
	case int:
		return int64(v), true
	case int64:
		return v, true
	default:
		return 0, false
	}
}

func (l literal) equal(v protoreflect.Value) bool {
	switch l.kind {
	case protoreflect.StringKind:
		return v.String() == l.str
	case protoreflect.BoolKind:
		return v.Bool() == l.b
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind,
		protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return v.Int() == l.i
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind,
		protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return v.Uint() == l.u
	case protoreflect.FloatKind, protoreflect.DoubleKind:
		return v.Float() == l.f
	case protoreflect.EnumKind:
		return v.Enum() == l.enum
	}
	return false
}

func (r msgRule) eval(root protoreflect.Message) bool {
	if r.op == "present" {
		return r.hasPath(root) == r.want
	}
	// Walk intermediates; Get on an unset message field yields an empty
	// read-only message, so unset leaves evaluate as proto defaults.
	msg := root
	for _, fd := range r.path[:len(r.path)-1] {
		msg = msg.Get(fd).Message()
	}
	leaf := r.path[len(r.path)-1]
	val := msg.Get(leaf)
	switch r.op {
	case "eq":
		return r.lit.equal(val)
	case "ne":
		return !r.lit.equal(val)
	case "in":
		for _, l := range r.list {
			if l.equal(val) {
				return true
			}
		}
		return false
	case "matches":
		return r.re.MatchString(val.String())
	case "contains":
		list := val.List()
		for i := 0; i < list.Len(); i++ {
			if r.lit.equal(list.Get(i)) {
				return true
			}
		}
		return false
	}
	return false
}

func (r msgRule) hasPath(root protoreflect.Message) bool {
	msg := root
	for _, fd := range r.path[:len(r.path)-1] {
		if !msg.Has(fd) {
			return false
		}
		msg = msg.Get(fd).Message()
	}
	return msg.Has(r.path[len(r.path)-1])
}
```

- [ ] **Step 4: Fetch grpc dependency and run the tests**

```bash
go get google.golang.org/grpc@latest
go test ./internal/match/ -v
```

Expected: PASS (all subtests in both test functions).

- [ ] **Step 5: Commit**

```bash
git add internal/match/ go.mod go.sum
git commit -m "feat: structured matcher engine with compile-time validation"
```

---

### Task 5: Stub types, response builder, and per-stub compilation

**Files:**
- Create: `internal/stub/stub.go`
- Test: `internal/stub/stub_test.go`

Design note: response messages are built by converting the YAML map to JSON and running first-party `protojson.Unmarshal` into a `dynamicpb` message. This buys protobuf-JSON fidelity for free (enum names, nested/repeated/map fields, 64-bit ints as numbers or strings, well-known types like `Timestamp` in RFC 3339 form) and makes "response doesn't fit the schema" a load-time error.

- [ ] **Step 1: Write the failing test**

`internal/stub/stub_test.go`:

```go
package stub

import (
	"context"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/yinghanhung/simulacra/internal/schema"
)

func testRegistry(t *testing.T) *schema.Registry {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatalf("AddProtoDir: %v", err)
	}
	return reg
}

func TestCompileBuildsResponse(t *testing.T) {
	reg := testRegistry(t)
	s := Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Respond: Respond{Message: map[string]any{
			"order_id": "o-123",
			"status":   "ORDER_STATUS_SHIPPED",
			"note":     "on its way",
			"eta":      "2026-08-01T12:00:00Z", // Timestamp in natural RFC 3339 form
		}},
	}
	c, err := Compile(reg, s, "test.yaml#0")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if c.Method != "/shop.v1.OrderService/GetOrder" {
		t.Errorf("Method = %q, want normalized /shop.v1.OrderService/GetOrder", c.Method)
	}
	out, err := protojson.Marshal(c.Response())
	if err != nil {
		t.Fatalf("marshaling response: %v", err)
	}
	for _, want := range []string{`"o-123"`, `ORDER_STATUS_SHIPPED`, `2026-08-01T12:00:00Z`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("response %s missing %s", out, want)
		}
	}
}

func TestCompileEmptyResponseIsValid(t *testing.T) {
	// A stub may legitimately respond with an empty/default message.
	reg := testRegistry(t)
	if _, err := Compile(reg, Stub{Method: "shop.v1.OrderService/GetOrder"}, "t#0"); err != nil {
		t.Fatalf("Compile with empty respond: %v", err)
	}
}

func TestCompileErrors(t *testing.T) {
	reg := testRegistry(t)
	cases := []struct {
		name string
		s    Stub
		want string // substring of the error
	}{
		{"unknown method", Stub{Method: "shop.v1.OrderService/Nope"}, "Nope"},
		{"streaming method", Stub{Method: "shop.v1.OrderService/WatchOrder"}, "unary"},
		{"response field not in schema", Stub{
			Method:  "shop.v1.OrderService/GetOrder",
			Respond: Respond{Message: map[string]any{"no_such_field": 1}},
		}, "no_such_field"},
		{"response enum typo", Stub{
			Method:  "shop.v1.OrderService/GetOrder",
			Respond: Respond{Message: map[string]any{"status": "SHIPPED_TYPO"}},
		}, "SHIPPED_TYPO"},
		{"bad match block", Stub{
			Method: "shop.v1.OrderService/GetOrder",
			Match:  &matchBlockWithBadField,
		}, "no_such"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Compile(reg, tc.s, "t#0")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}
```

Add the helper variable at the bottom of the test file:

```go
import "github.com/yinghanhung/simulacra/internal/match" // add to imports

var matchBlockWithBadField = match.Block{
	Message: map[string]match.Rules{"no_such": {"eq": "x"}},
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/stub/ -v`
Expected: FAIL — package does not compile (`undefined: Stub`).

- [ ] **Step 3: Implement stub types and compilation**

`internal/stub/stub.go`:

```go
// Package stub defines the YAML stub format (M1 subset), compiles stubs
// against the schema registry, and selects them at request time.
package stub

import (
	"encoding/json"
	"fmt"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

// Stub is one entry in a stub YAML file (files hold a list of these).
type Stub struct {
	Method   string       `yaml:"method"`
	Match    *match.Block `yaml:"match"`
	Priority int          `yaml:"priority"`
	Times    int          `yaml:"times"` // 0 means unlimited
	Respond  Respond      `yaml:"respond"`
}

type Respond struct {
	Message map[string]any `yaml:"message"`
}

// Compiled is a stub validated against the schema: matcher compiled,
// response message pre-built. Source identifies where it came from
// ("path/to/file.yaml#index") for error messages.
type Compiled struct {
	Method   string // normalized "/pkg.Service/Method"
	Priority int
	Times    int
	Source   string
	matcher  *match.Compiled
	response *dynamicpb.Message
}

func (c *Compiled) Matches(msg protoreflect.Message, md metadata.MD) bool {
	return c.matcher.Eval(msg, md)
}

// Response returns the pre-built response message. It is shared across
// calls and must be treated as read-only.
func (c *Compiled) Response() *dynamicpb.Message { return c.response }

func Compile(reg *schema.Registry, s Stub, source string) (*Compiled, error) {
	m, err := reg.LookupMethod(s.Method)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	if m.IsStreamingClient() || m.IsStreamingServer() {
		return nil, fmt.Errorf("%s: %s is a streaming method; this build supports unary methods only", source, s.Method)
	}
	cm, err := match.Compile(m.Input(), s.Match)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	resp, err := BuildMessage(m.Output(), s.Respond.Message)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", source, err)
	}
	return &Compiled{
		Method:   fmt.Sprintf("/%s/%s", m.Parent().FullName(), m.Name()),
		Priority: s.Priority,
		Times:    s.Times,
		Source:   source,
		matcher:  cm,
		response: resp,
	}, nil
}

// BuildMessage turns a stub's YAML message body into a dynamic protobuf
// message via the canonical protobuf-JSON mapping (protojson), so enum
// names, nested/repeated/map fields, 64-bit ints, and well-known types
// (e.g. Timestamp as RFC 3339 strings) all behave per spec.
func BuildMessage(desc protoreflect.MessageDescriptor, fields map[string]any) (*dynamicpb.Message, error) {
	if fields == nil {
		fields = map[string]any{}
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return nil, fmt.Errorf("encoding response message as JSON: %w", err)
	}
	msg := dynamicpb.NewMessage(desc)
	if err := protojson.Unmarshal(data, msg); err != nil {
		return nil, fmt.Errorf("response message does not fit %s: %w", desc.FullName(), err)
	}
	return msg, nil
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/stub/ -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/stub/
git commit -m "feat: stub compilation with protojson-based response building"
```

---

### Task 6: Stub file loader and selection store

**Files:**
- Create: `internal/stub/loader.go`, `internal/stub/store.go`
- Test: `internal/stub/loader_test.go`, `internal/stub/store_test.go`

- [ ] **Step 1: Write the failing loader test**

`internal/stub/loader_test.go`:

```go
package stub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadDirs(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "orders.yaml", `
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "o-123" }
  respond:
    message: { order_id: "o-123", status: ORDER_STATUS_SHIPPED }
- method: shop.v1.OrderService/GetOrder
  priority: -1
  respond:
    message: { note: "fallback" }
`)
	stubs, errs := LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("LoadDirs errors: %v", errs)
	}
	if len(stubs) != 2 {
		t.Fatalf("got %d stubs, want 2", len(stubs))
	}
	if !strings.Contains(stubs[0].Source, "orders.yaml#0") {
		t.Errorf("Source = %q, want to contain orders.yaml#0", stubs[0].Source)
	}
}

func TestLoadDirsReportsAllErrors(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "bad.yaml", `
- method: shop.v1.OrderService/Nope
  respond: { message: {} }
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { no_such_field: 1 }
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2 (one per bad stub): %v", len(errs), errs)
	}
}

func TestLoadDirsRejectsUnknownKeys(t *testing.T) {
	// Strict parsing: M2 syntax (e.g. `delay:`) must fail loudly in M1.
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "future.yaml", `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: {}
    delay: 50ms
`)
	_, errs := LoadDirs(reg, []string{dir})
	if len(errs) == 0 {
		t.Fatal("expected an error for unknown key 'delay'")
	}
	if !strings.Contains(errs[0].Error(), "delay") {
		t.Errorf("error %q should mention the unknown key", errs[0])
	}
}

func TestLoadDirsRejectsInvalidYAML(t *testing.T) {
	reg := testRegistry(t)
	dir := t.TempDir()
	writeFile(t, dir, "broken.yaml", "{{{ not yaml")
	if _, errs := LoadDirs(reg, []string{dir}); len(errs) == 0 {
		t.Fatal("expected a parse error")
	}
}
```

- [ ] **Step 2: Write the failing store test**

`internal/stub/store_test.go`:

```go
package stub

import (
	"context"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/match"
	"github.com/yinghanhung/simulacra/internal/schema"
)

const method = "/shop.v1.OrderService/GetOrder"

func compiled(t *testing.T, reg *schema.Registry, s Stub) *Compiled {
	t.Helper()
	c, err := Compile(reg, s, "test")
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	return c
}

func request(t *testing.T, reg *schema.Registry, jsonBody string) *dynamicpb.Message {
	t.Helper()
	m, err := reg.LookupMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	msg := dynamicpb.NewMessage(m.Input())
	if err := protojson.Unmarshal([]byte(jsonBody), msg); err != nil {
		t.Fatal(err)
	}
	return msg
}

func TestSelectPriorityAndTimes(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	specific := compiled(t, reg, Stub{
		Method:   "shop.v1.OrderService/GetOrder",
		Match:    &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "o-123"}}},
		Priority: 10,
		Times:    1,
		Respond:  Respond{Message: map[string]any{"note": "specific"}},
	})
	fallback := compiled(t, reg, Stub{
		Method:  "shop.v1.OrderService/GetOrder",
		Respond: Respond{Message: map[string]any{"note": "fallback"}},
	})
	store := NewStore([]*Compiled{fallback, specific}) // load order: fallback first

	req := request(t, reg, `{"order_id":"o-123"}`)

	// Higher priority wins even though it was loaded second.
	if got := store.Select(method, req.ProtoReflect(), nil); got != specific {
		t.Fatalf("first select = %v, want the priority-10 stub", got)
	}
	// times: 1 is now exhausted; fallback matches next.
	if got := store.Select(method, req.ProtoReflect(), nil); got != fallback {
		t.Fatalf("second select = %v, want the fallback stub", got)
	}
	// No stubs for other methods.
	if got := store.Select("/shop.v1.OrderService/Other", req.ProtoReflect(), nil); got != nil {
		t.Fatalf("select for unknown method = %v, want nil", got)
	}
	if n := store.CountFor(method); n != 2 {
		t.Errorf("CountFor = %d, want 2", n)
	}
}

func TestSelectNoMatch(t *testing.T) {
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	only := compiled(t, reg, Stub{
		Method: "shop.v1.OrderService/GetOrder",
		Match:  &match.Block{Message: map[string]match.Rules{"order_id": {"eq": "o-999"}}},
	})
	store := NewStore([]*Compiled{only})
	req := request(t, reg, `{"order_id":"o-123"}`)
	if got := store.Select(method, req.ProtoReflect(), nil); got != nil {
		t.Fatalf("Select = %v, want nil for non-matching request", got)
	}
}
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `go test ./internal/stub/ -v`
Expected: FAIL — `undefined: LoadDirs`, `undefined: NewStore`.

- [ ] **Step 4: Implement the loader**

`internal/stub/loader.go`:

```go
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
```

- [ ] **Step 5: Implement the store**

`internal/stub/store.go`:

```go
package stub

import (
	"sort"
	"sync"

	"google.golang.org/grpc/metadata"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// Store holds compiled stubs grouped by method and selects the stub for a
// request: highest priority first (load order breaks ties), skipping stubs
// whose `times` budget is spent. Selection consumes one use atomically.
type Store struct {
	mu       sync.Mutex
	byMethod map[string][]*entry
}

type entry struct {
	stub *Compiled
	used int
}

func NewStore(stubs []*Compiled) *Store {
	s := &Store{byMethod: make(map[string][]*entry)}
	for _, c := range stubs {
		s.byMethod[c.Method] = append(s.byMethod[c.Method], &entry{stub: c})
	}
	for _, entries := range s.byMethod {
		sort.SliceStable(entries, func(i, j int) bool {
			return entries[i].stub.Priority > entries[j].stub.Priority
		})
	}
	return s
}

// Select returns the first live matching stub for the method, or nil.
func (s *Store) Select(method string, msg protoreflect.Message, md metadata.MD) *Compiled {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range s.byMethod[method] {
		if e.stub.Times > 0 && e.used >= e.stub.Times {
			continue
		}
		if e.stub.Matches(msg, md) {
			e.used++
			return e.stub
		}
	}
	return nil
}

// CountFor reports how many stubs are registered for a method (regardless
// of times budget) — used in "no stub matched" error messages.
func (s *Store) CountFor(method string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.byMethod[method])
}
```

- [ ] **Step 6: Fetch yaml dependency and run the tests**

```bash
go get gopkg.in/yaml.v3@latest
go test ./internal/stub/ -v
```

Expected: PASS (all tests in the package, including Task 5's).

- [ ] **Step 7: Commit**

```bash
git add internal/stub/ go.mod go.sum
git commit -m "feat: strict YAML stub loading and priority/times selection store"
```

---

### Task 7: Dynamic data plane — serve unary calls

**Files:**
- Create: `internal/dataplane/server.go`
- Test: `internal/dataplane/server_test.go`

- [ ] **Step 1: Write the failing test**

`internal/dataplane/server_test.go` — starts a real server on a random port and drives it with a dynamic (codegen-free) gRPC client:

```go
package dataplane

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

const stubsYAML = `
- method: shop.v1.OrderService/GetOrder
  match:
    metadata:
      x-tenant: { eq: "acme" }
    message:
      order_id: { eq: "o-123" }
  respond:
    message: { order_id: "o-123", status: ORDER_STATUS_SHIPPED, note: "hello" }
`

func startServer(t *testing.T) (*schema.Registry, *grpc.ClientConn) {
	t.Helper()
	reg := schema.NewRegistry()
	if err := reg.AddProtoDir(context.Background(), "../../testdata/protos"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := writeStubFile(dir, stubsYAML); err != nil {
		t.Fatal(err)
	}
	stubs, errs := stub.LoadDirs(reg, []string{dir})
	if len(errs) > 0 {
		t.Fatalf("stub load errors: %v", errs)
	}
	srv, err := New(reg, stub.NewStore(stubs))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.GracefulStop)

	conn, err := grpc.NewClient(lis.Addr().String(),
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return reg, conn
}

func invoke(t *testing.T, reg *schema.Registry, conn *grpc.ClientConn, ctx context.Context, method, reqJSON string) (string, error) {
	t.Helper()
	m, err := reg.LookupMethod(method)
	if err != nil {
		t.Fatal(err)
	}
	req := dynamicpb.NewMessage(m.Input())
	if err := protojson.Unmarshal([]byte(reqJSON), req); err != nil {
		t.Fatal(err)
	}
	resp := dynamicpb.NewMessage(m.Output())
	if err := conn.Invoke(ctx, method, req, resp); err != nil {
		return "", err
	}
	out, err := protojson.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	return string(out), nil
}

func TestUnaryMatchedCall(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx, "x-tenant", "acme")

	out, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"o-123"}`)
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	for _, want := range []string{"o-123", "ORDER_STATUS_SHIPPED", "hello"} {
		if !strings.Contains(out, want) {
			t.Errorf("response %s missing %q", out, want)
		}
	}
}

func TestUnaryNoStubMatched(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Right method, but metadata is missing → no stub matches.
	_, err := invoke(t, reg, conn, ctx, "/shop.v1.OrderService/GetOrder", `{"order_id":"o-123"}`)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.NotFound {
		t.Fatalf("err = %v, want NotFound", err)
	}
	if !strings.Contains(st.Message(), "1 stub") {
		t.Errorf("message %q should mention how many stubs exist for the method", st.Message())
	}
}

func TestUnknownMethodIsUnimplemented(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Any proto message works as a payload; the server must reject the
	// method name before it ever decodes the body.
	m, _ := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	req := dynamicpb.NewMessage(m.Input())
	resp := dynamicpb.NewMessage(m.Output())
	err := conn.Invoke(ctx, "/no.such.Service/Nope", req, resp)
	if st, ok := status.FromError(err); !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented", err)
	}
}

func TestStreamingMethodRejected(t *testing.T) {
	reg, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, _ := reg.LookupMethod("/shop.v1.OrderService/GetOrder")
	req := dynamicpb.NewMessage(m.Input())
	resp := dynamicpb.NewMessage(m.Output())
	err := conn.Invoke(ctx, "/shop.v1.OrderService/WatchOrder", req, resp)
	st, ok := status.FromError(err)
	if !ok || st.Code() != codes.Unimplemented {
		t.Fatalf("err = %v, want Unimplemented for streaming method in M1", err)
	}
	if !strings.Contains(st.Message(), "streaming") {
		t.Errorf("message %q should explain streaming is not supported yet", st.Message())
	}
}
```

Add the small file-writing helper at the bottom of the test file (and add `"os"` and `"path/filepath"` to the existing import block):

```go
func writeStubFile(dir, content string) error {
	return os.WriteFile(filepath.Join(dir, "stubs.yaml"), []byte(content), 0o644)
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/dataplane/ -v`
Expected: FAIL — `undefined: New` (package doesn't exist yet).

- [ ] **Step 3: Implement the server**

`internal/dataplane/server.go`:

```go
// Package dataplane serves the mock gRPC endpoint: a single grpc-go server
// whose UnknownServiceHandler dispatches every method of every registered
// schema dynamically — no code generation, no per-service registration.
package dataplane

import (
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/reflection"
	v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"
	v1alphareflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

type Server struct {
	reg   *schema.Registry
	store *stub.Store
	grpc  *grpc.Server
}

func New(reg *schema.Registry, store *stub.Store) (*Server, error) {
	s := &Server{reg: reg, store: store}
	s.grpc = grpc.NewServer(grpc.UnknownServiceHandler(s.handleUnknown))

	// Health: standard grpc.health.v1 protocol, always SERVING in M1.
	hs := health.NewServer()
	healthpb.RegisterHealthServer(s.grpc, hs)
	hs.SetServingStatus("", healthpb.HealthCheckResponse_SERVING)

	// Make the health service resolvable through our reflection resolver
	// so `grpcurl ... grpc.health.v1.Health/Check` works against the mock.
	if err := reg.AddFile(healthpb.File_grpc_health_v1_health_proto); err != nil {
		return nil, err
	}

	// Reflection over the dynamic registry: v1 and v1alpha (grpcurl uses both).
	opts := reflection.ServerOptions{
		Services:           s,
		DescriptorResolver: reg.Files(),
	}
	v1reflectionpb.RegisterServerReflectionServer(s.grpc, reflection.NewServerV1(opts))
	v1alphareflectionpb.RegisterServerReflectionServer(s.grpc, reflection.NewServer(opts))
	return s, nil
}

// GetServiceInfo implements reflection.ServiceInfoProvider by listing every
// service in the schema registry (mocked services plus health).
func (s *Server) GetServiceInfo() map[string]grpc.ServiceInfo {
	out := make(map[string]grpc.ServiceInfo)
	for _, svc := range s.reg.Services() {
		out[string(svc.FullName())] = grpc.ServiceInfo{Metadata: svc.ParentFile().Path()}
	}
	return out
}

func (s *Server) Serve(lis net.Listener) error { return s.grpc.Serve(lis) }
func (s *Server) GracefulStop()                { s.grpc.GracefulStop() }

func (s *Server) handleUnknown(_ any, stream grpc.ServerStream) error {
	full, ok := grpc.MethodFromServerStream(stream)
	if !ok {
		return status.Error(codes.Internal, "simulacra: no method name on stream")
	}
	m, err := s.reg.LookupMethod(full)
	if err != nil {
		return status.Errorf(codes.Unimplemented, "simulacra: %v", err)
	}
	if m.IsStreamingClient() || m.IsStreamingServer() {
		return status.Errorf(codes.Unimplemented,
			"simulacra: %s is a streaming method; this build supports unary methods only (streaming lands in M2)", full)
	}

	req := dynamicpb.NewMessage(m.Input())
	if err := stream.RecvMsg(req); err != nil {
		return status.Errorf(codes.Internal, "simulacra: receiving request: %v", err)
	}
	md, _ := metadata.FromIncomingContext(stream.Context())

	selected := s.store.Select(full, req.ProtoReflect(), md)
	if selected == nil {
		return status.Errorf(codes.NotFound,
			"simulacra: no stub matched %s (%d stub(s) registered for this method)",
			full, s.store.CountFor(full))
	}
	return stream.SendMsg(selected.Response())
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/dataplane/ -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Run the full suite with the race detector**

Run: `go test -race ./...`
Expected: PASS, no data races.

- [ ] **Step 6: Commit**

```bash
git add internal/dataplane/
git commit -m "feat: dynamic gRPC data plane serving stubbed unary calls"
```

---

### Task 8: Reflection and health end-to-end

**Files:**
- Test: `internal/dataplane/server_test.go` (append)

The wiring already exists in Task 7's `New`; this task proves it works against real clients — the part grpcurl depends on.

- [ ] **Step 1: Write the failing tests**

Append to `internal/dataplane/server_test.go` (add imports `v1reflectionpb "google.golang.org/grpc/reflection/grpc_reflection_v1"` and `healthpb "google.golang.org/grpc/health/grpc_health_v1"`):

```go
func TestReflectionListsAndResolvesServices(t *testing.T) {
	_, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	rc := v1reflectionpb.NewServerReflectionClient(conn)
	strm, err := rc.ServerReflectionInfo(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// ListServices must include the mocked service and health.
	if err := strm.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_ListServices{ListServices: ""},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err := strm.Recv()
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, s := range resp.GetListServicesResponse().GetService() {
		names[s.GetName()] = true
	}
	for _, want := range []string{"shop.v1.OrderService", "grpc.health.v1.Health"} {
		if !names[want] {
			t.Errorf("ListServices missing %s (got %v)", want, names)
		}
	}

	// FileContainingSymbol must return descriptor bytes for the mocked service.
	if err := strm.Send(&v1reflectionpb.ServerReflectionRequest{
		MessageRequest: &v1reflectionpb.ServerReflectionRequest_FileContainingSymbol{
			FileContainingSymbol: "shop.v1.OrderService",
		},
	}); err != nil {
		t.Fatal(err)
	}
	resp, err = strm.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.GetFileDescriptorResponse().GetFileDescriptorProto()) == 0 {
		t.Error("FileContainingSymbol returned no descriptors for shop.v1.OrderService")
	}
}

func TestHealthCheck(t *testing.T) {
	_, conn := startServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{})
	if err != nil {
		t.Fatalf("health check: %v", err)
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		t.Errorf("status = %v, want SERVING", resp.GetStatus())
	}
}
```

- [ ] **Step 2: Run the tests**

Run: `go test ./internal/dataplane/ -v`
Expected: PASS. (If `TestReflectionListsAndResolvesServices` fails on missing services or unresolvable symbols, the bug is in Task 7's reflection wiring — `ServerOptions.DescriptorResolver` must be the registry's `Files()` and `GetServiceInfo` must enumerate registry services.)

- [ ] **Step 3: Commit**

```bash
git add internal/dataplane/
git commit -m "test: prove reflection and health work against real clients"
```

---

### Task 9: CLI — `serve` and `check`

**Files:**
- Create: `internal/cli/root.go`, `internal/cli/load.go`, `internal/cli/serve.go`, `internal/cli/check.go`
- Modify: `cmd/simulacra/main.go`
- Test: `internal/cli/check_test.go`

- [ ] **Step 1: Write the failing test for `check`**

`internal/cli/check_test.go`:

```go
package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newRootCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestCheckValidStubs(t *testing.T) {
	dir := t.TempDir()
	stubs := `
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { order_id: "x" }
`
	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte(stubs), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "check", "--proto", "../../testdata/protos", "--stubs", dir)
	if err != nil {
		t.Fatalf("check failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 stub") {
		t.Errorf("output %q should summarize the stub count", out)
	}
}

func TestCheckReportsEveryError(t *testing.T) {
	dir := t.TempDir()
	stubs := `
- method: shop.v1.OrderService/Nope
  respond: { message: {} }
- method: shop.v1.OrderService/GetOrder
  respond:
    message: { bad_field: 1 }
`
	if err := os.WriteFile(filepath.Join(dir, "s.yaml"), []byte(stubs), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := run(t, "check", "--proto", "../../testdata/protos", "--stubs", dir)
	if err == nil {
		t.Fatal("check should fail for invalid stubs")
	}
	if !strings.Contains(out, "Nope") || !strings.Contains(out, "bad_field") {
		t.Errorf("output should mention both errors, got:\n%s", out)
	}
}

func TestCheckRequiresSchemaSource(t *testing.T) {
	if _, err := run(t, "check", "--stubs", t.TempDir()); err == nil {
		t.Fatal("check without --proto/--descriptors should fail")
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/cli/ -v`
Expected: FAIL — `undefined: newRootCmd`.

- [ ] **Step 3: Implement the CLI**

`internal/cli/root.go`:

```go
// Package cli wires the simulacra command-line interface.
package cli

import (
	"os"

	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "simulacra",
		Short:         "A gRPC-native mock server",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.AddCommand(newServeCmd(), newCheckCmd())
	return root
}

func Execute() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		root.PrintErrln("error:", err)
		os.Exit(1)
	}
}
```

`internal/cli/load.go`:

```go
package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// schemaFlags and stubFlags are shared by `serve` and `check`.
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
```

`internal/cli/check.go`:

```go
package cli

import (
	"errors"

	"github.com/spf13/cobra"
)

func newCheckCmd() *cobra.Command {
	src := &sources{}
	cmd := &cobra.Command{
		Use:   "check",
		Short: "Validate stub files against schemas without starting a server",
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(src.stubDirs) == 0 {
				return errors.New("--stubs <dir> is required")
			}
			reg, err := src.buildRegistry(cmd.Context())
			if err != nil {
				return err
			}
			stubs, err := src.loadStubs(cmd, reg)
			if err != nil {
				return err
			}
			cmd.Printf("OK: %d stub(s) validated against %d service(s)\n",
				len(stubs), len(reg.Services()))
			return nil
		},
	}
	src.register(cmd)
	return cmd
}
```

`internal/cli/serve.go`:

```go
package cli

import (
	"fmt"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/stub"
)

func newServeCmd() *cobra.Command {
	src := &sources{}
	var listen string
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Start the mock gRPC server",
		RunE: func(cmd *cobra.Command, args []string) error {
			reg, err := src.buildRegistry(cmd.Context())
			if err != nil {
				return err
			}
			stubs, err := src.loadStubs(cmd, reg)
			if err != nil {
				return err
			}
			srv, err := dataplane.New(reg, stub.NewStore(stubs))
			if err != nil {
				return err
			}
			lis, err := net.Listen("tcp", listen)
			if err != nil {
				return fmt.Errorf("listening on %s: %w", listen, err)
			}

			cmd.Printf("simulacra: data plane listening on %s\n", lis.Addr())
			cmd.Printf("  %d service(s) registered, %d stub(s) loaded — reflection and health enabled\n",
				len(reg.Services()), len(stubs))

			sig := make(chan os.Signal, 1)
			signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
			go func() {
				<-sig
				cmd.Println("simulacra: shutting down")
				srv.GracefulStop()
			}()
			return srv.Serve(lis)
		},
	}
	src.register(cmd)
	cmd.Flags().StringVar(&listen, "listen", ":6565", "data-plane listen address")
	return cmd
}
```

`cmd/simulacra/main.go` (replace the placeholder):

```go
package main

import "github.com/yinghanhung/simulacra/internal/cli"

func main() {
	cli.Execute()
}
```

- [ ] **Step 4: Fetch cobra and run the tests**

```bash
go get github.com/spf13/cobra@latest
go test ./internal/cli/ -v
```

Expected: PASS (all three check tests).

- [ ] **Step 5: Build and smoke-test the binary by hand**

```bash
go build -o simulacra ./cmd/simulacra
./simulacra check --proto ./testdata/protos --stubs /tmp/does-not-matter 2>&1 || true
./simulacra --help
```

Expected: `--help` lists `serve` and `check`; the bad `check` prints a clear error and exits non-zero.

- [ ] **Step 6: Commit**

```bash
git add internal/cli/ cmd/ go.mod go.sum
git commit -m "feat: simulacra serve and check commands"
```

---

### Task 10: CI and the exit-criterion verification

**Files:**
- Create: `.github/workflows/ci.yml`

- [ ] **Step 1: Write the CI workflow**

`.github/workflows/ci.yml`:

```yaml
name: ci
on:
  push:
    branches: [main]
  pull_request:

jobs:
  test:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25"
      - run: go build ./...
      - run: go vet ./...
      - run: go test -race ./...
```

- [ ] **Step 2: Run the same gates locally**

Run: `go build ./... && go vet ./... && go test -race ./...`
Expected: all pass.

- [ ] **Step 3: Verify the M1 exit criterion with real grpcurl (manual)**

```bash
# Terminal 1 — write a stub and serve:
mkdir -p /tmp/simulacra-demo/stubs
cat > /tmp/simulacra-demo/stubs/orders.yaml <<'EOF'
- method: shop.v1.OrderService/GetOrder
  match:
    message:
      order_id: { eq: "o-123" }
  respond:
    message:
      order_id: "o-123"
      status: ORDER_STATUS_SHIPPED
      note: "mocked by simulacra"
EOF
go run ./cmd/simulacra serve --proto ./testdata/protos --stubs /tmp/simulacra-demo/stubs

# Terminal 2 — grpcurl discovers the schema via reflection and calls the mock:
grpcurl -plaintext localhost:6565 list
grpcurl -plaintext -d '{"order_id":"o-123"}' localhost:6565 shop.v1.OrderService/GetOrder
grpcurl -plaintext -d '{"order_id":"nope"}' localhost:6565 shop.v1.OrderService/GetOrder
grpcurl -plaintext localhost:6565 grpc.health.v1.Health/Check
```

Expected:
- `list` shows `shop.v1.OrderService` and `grpc.health.v1.Health`.
- The `o-123` call returns `{"orderId":"o-123","status":"ORDER_STATUS_SHIPPED","note":"mocked by simulacra"}`.
- The `nope` call fails with `NotFound` and a message naming the method and stub count.
- The health call returns `{"status":"SERVING"}`.

(If `grpcurl` is not installed: `brew install grpcurl`.)

- [ ] **Step 4: Commit**

```bash
git add .github/
git commit -m "ci: build, vet, and race-tested suite on push and PR"
```

---

## Done means

- `go test -race ./...` passes.
- The manual grpcurl session in Task 10 behaves exactly as documented — that is the M1 exit criterion from PROPOSAL.md §12.
- Everything listed under "M1 scope guard" is still absent (no scope creep).
