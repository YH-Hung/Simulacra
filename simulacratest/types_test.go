package simulacratest_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/dynamicpb"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
	shopv1 "github.com/yinghanhung/simulacra/internal/testpb/shop/v1"
	"github.com/yinghanhung/simulacra/simulacratest"
)

const legacyPath = "conformance/v1/legacy.proto"

// identityRefusal is RegisterFiles' extension-identity refusal for path.
func identityRefusal(path string) string {
	return "simulacratest: RegisterFiles: " + path + " declares extensions and is already registered with this Client as a different descriptor instance; extensions are matched by descriptor identity, so values decoded through the registered copy would be invisible through this one — register one instance and build extension values from it"
}

// instanceMismatch is checkExtensionInstances' error for path.
func instanceMismatch(path string) string {
	return "message built from a different descriptor instance of " + path + " than the one registered with this Client; build messages from the FileDescriptor passed to RegisterFiles"
}

func newMessage(fd protoreflect.FileDescriptor, name protoreflect.Name) *dynamicpb.Message {
	return dynamicpb.NewMessage(fd.Messages().ByName(name))
}

func extension(fd protoreflect.FileDescriptor, name protoreflect.Name) protoreflect.ExtensionTypeDescriptor {
	return dynamicpb.NewExtensionType(fd.Extensions().ByName(name)).TypeDescriptor()
}

func serviceNames(t *testing.T, sim *simulacratest.Server) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := sim.Admin().Schema.ListServices(ctx, connect.NewRequest(&adminv1.ListServicesRequest{}))
	if err != nil {
		t.Fatalf("ListServices: %v", err)
	}
	var names []string
	for _, s := range resp.Msg.GetServices() {
		names = append(names, s.GetName())
	}
	return names
}

func TestRegisterFilesRegistersTheClosureOnTheServer(t *testing.T) {
	sim := simulacratest.Start(t)
	files := conformanceFiles(t)
	// nulls.proto imports legacy.proto, so LegacyService on the server proves
	// the import went with it. Passing legacy.proto as well, from the same
	// compile, reaches it twice as one instance, which must not be refused.
	sim.RegisterFiles(t, files["conformance/v1/nulls.proto"], files[legacyPath])
	got := serviceNames(t, sim)
	for _, want := range []string{"conformance.v1.NullService", "conformance.v1.LegacyService"} {
		if !slices.Contains(got, want) {
			t.Fatalf("ListServices = %v; want %s", got, want)
		}
	}
}

func TestRegisterFilesOfGeneratedShopOntoAProtoDirServerAddsNothing(t *testing.T) {
	sim := simulacratest.Start(t, simulacratest.WithProtoDir("../testdata/protos"))
	counter := countSchemaRPCs(sim.Client)
	sim.RegisterFiles(t, shopv1.File_shop_v1_order_proto)
	if counter.registers != 1 || counter.last == nil || len(counter.last.GetRegisteredFiles()) != 0 {
		t.Fatalf("RegisterSchemas calls = %d, last response = %v; want one call that added nothing", counter.registers, counter.last)
	}
}

func TestRegisterFilesRefusesASecondInstanceOfAnExtensionFile(t *testing.T) {
	sim := simulacratest.Start(t)
	counter := countSchemaRPCs(sim.Client)
	base, e1, e2 := sharedExtendee(t)
	// An extension declared inside a message counts as much as one at file
	// scope.
	const holderText = `name: "probe/holder.proto" package: "probe" syntax: "proto2"
dependency: "probe/base.proto"
message_type { name: "Holder" extension { name: "note" number: 101 label: LABEL_OPTIONAL type: TYPE_STRING extendee: ".probe.Base" } }`
	h1, h2 := buildFile(t, holderText, base), buildFile(t, holderText, base)

	sim.RegisterFiles(t, base, e1, h1)
	sent := counter.registers
	for _, fd := range []protoreflect.FileDescriptor{e2, h2} {
		rec := runRecorded(t, func(tb testing.TB) { sim.RegisterFiles(tb, fd) })
		if want := identityRefusal(fd.Path()); rec.FatalMsg() != want {
			t.Errorf("RegisterFiles(second %s) fatal = %q; want %q", fd.Path(), rec.FatalMsg(), want)
		}
	}
	if counter.registers != sent {
		t.Fatalf("RegisterSchemas calls = %d after the refusals; want %d (refused before any RPC)", counter.registers, sent)
	}
}

func TestRegisterFilesRefusesTwoInstancesOfAnExtensionFileInOneCall(t *testing.T) {
	sim := simulacratest.Start(t)
	counter := countSchemaRPCs(sim.Client)
	base, e1, e2 := sharedExtendee(t)
	rec := runRecorded(t, func(tb testing.TB) { sim.RegisterFiles(tb, base, e1, e2) })
	if want := identityRefusal("probe/ext.proto"); rec.FatalMsg() != want || counter.registers != 0 {
		t.Fatalf("fatal = %q after %d RegisterSchemas calls; want %q after none", rec.FatalMsg(), counter.registers, want)
	}
}

func TestRegisterFilesRefusesAnInstanceThatDiffersFromTheLinkedCopy(t *testing.T) {
	sim := simulacratest.Start(t)
	base, e1, e2 := sharedExtendee(t)
	linked := new(protoregistry.Files)
	for _, fd := range []protoreflect.FileDescriptor{base, e1} {
		if err := linked.RegisterFile(fd); err != nil {
			t.Fatalf("registering %s: %v", fd.Path(), err)
		}
	}
	simulacratest.SetGlobalFiles(sim.Client, linked)
	rec := runRecorded(t, func(tb testing.TB) { sim.RegisterFiles(tb, e2) })
	if want := identityRefusal("probe/ext.proto"); rec.FatalMsg() != want {
		t.Fatalf("fatal = %q; want %q", rec.FatalMsg(), want)
	}
}

func TestRegisterFilesAcceptsTheSameInstanceAndDistinctNonExtensionFiles(t *testing.T) {
	sim := simulacratest.Start(t)
	_, e1, _ := sharedExtendee(t)
	sim.RegisterFiles(t, e1)
	sim.RegisterFiles(t, e1)
	// corpus.proto declares no extensions, so a second instance keeps the
	// first-registration-wins skip.
	sim.RegisterFiles(t, conformanceFiles(t)["conformance/v1/corpus.proto"])
	sim.RegisterFiles(t, conformanceFiles(t)["conformance/v1/corpus.proto"])
}

func TestCheckExtensionInstances(t *testing.T) {
	sim := simulacratest.Start(t)
	a, b := conformanceFiles(t), conformanceFiles(t)
	sim.RegisterFiles(t, a[legacyPath])
	sent := newMessage(a[legacyPath], "LegacyReply")
	sent.Set(extension(a[legacyPath], "ext_tag"), protoreflect.ValueOfString("t"))
	wire, err := proto.Marshal(sent)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	decode := func(t *testing.T, into *dynamicpb.Message) {
		t.Helper()
		if err := simulacratest.DecodeWithClientTypes(sim.Client, wire, into); err != nil {
			t.Fatalf("decode: %v", err)
		}
	}

	t.Run("different extendee instance", func(t *testing.T) {
		got := newMessage(b[legacyPath], "LegacyReply")
		decode(t, got)
		if err := simulacratest.CheckExtensionInstances(got); err == nil || err.Error() != instanceMismatch(legacyPath) {
			t.Fatalf("CheckExtensionInstances = %v (Has through its own ext_tag: %v); want %q",
				err, got.Has(extension(b[legacyPath], "ext_tag")), instanceMismatch(legacyPath))
		}
	})

	t.Run("registered instance", func(t *testing.T) {
		got := newMessage(a[legacyPath], "LegacyReply")
		decode(t, got)
		if err := simulacratest.CheckExtensionInstances(got); err != nil {
			t.Fatalf("CheckExtensionInstances = %v; want nil", err)
		}
		if !got.Has(extension(a[legacyPath], "ext_tag")) {
			t.Fatalf("ext_tag not visible through the registered instance")
		}
	})

	t.Run("nested in a repeated field", func(t *testing.T) {
		x, y := nestedExtFile(t), nestedExtFile(t)
		sim.RegisterFiles(t, x)
		outer := newMessage(y, "Outer")
		item := newMessage(y, "Base")
		item.Set(extension(y, "tag"), protoreflect.ValueOfString("t"))
		items := outer.Mutable(y.Messages().ByName("Outer").Fields().ByName("items")).List()
		items.Append(protoreflect.ValueOfMessage(item))
		wire, err := proto.Marshal(outer)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		got := newMessage(y, "Outer")
		if err := simulacratest.DecodeWithClientTypes(sim.Client, wire, got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := simulacratest.CheckExtensionInstances(got); err == nil || err.Error() != instanceMismatch("nested/x.proto") {
			t.Fatalf("CheckExtensionInstances = %v; want %q", err, instanceMismatch("nested/x.proto"))
		}
	})
}

func TestLocalRegistryIsSafeUnderConcurrentRegistration(t *testing.T) {
	sim := simulacratest.Start(t)
	sim.RegisterFiles(t, conformanceFiles(t)[legacyPath])
	// Readers resolve through whichever snapshot is current while the
	// registrations below replace it; every snapshot must still hold
	// legacy.proto's extension.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	for range 4 {
		readers.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, err := simulacratest.ResolveExtension(sim.Client, "conformance.v1.LegacyReply", 100); err != nil {
					t.Errorf("ResolveExtension: %v", err)
					return
				}
			}
		})
	}
	t.Run("register", func(t *testing.T) {
		for i := range 8 {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				fds := make([]protoreflect.FileDescriptor, 4)
				for j := range fds {
					fds[j] = syntheticFile(t, i*4+j)
				}
				sim.RegisterFiles(t, fds...)
			})
		}
	})
	close(stop)
	readers.Wait()
}

func TestConcurrentRegisterFilesAllLand(t *testing.T) {
	// A lost update needs a registration to load the snapshot in the instant
	// between another's unlock and swap, so each goroutine registers several
	// times to give that window many chances to open.
	const goroutines, each = 16, 8
	sim := simulacratest.Start(t)
	t.Run("register", func(t *testing.T) {
		for i := range goroutines {
			t.Run(strconv.Itoa(i), func(t *testing.T) {
				t.Parallel()
				for j := range each {
					sim.RegisterFiles(t, syntheticFile(t, i*each+j))
				}
			})
		}
	})
	var want []string
	for i := range goroutines * each {
		want = append(want, fmt.Sprintf("syn/file_%d.proto", i))
	}
	slices.Sort(want)
	if got := simulacratest.LocalPaths(sim.Client); !slices.Equal(got, want) {
		t.Fatalf("local paths = %v; want %v", got, want)
	}
}
