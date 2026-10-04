// Package simulacratest is a testing.T-integrated Go SDK for Simulacra:
// Start boots an in-process server, and the helpers on Client create stubs,
// verify and capture the calls the server received, reset state, and dial the
// data plane.
//
// Everything goes through the admin API over loopback, in-process included.
// The SDK never touches the server's store, registry, or journal directly, so
// a test exercises the same surface as the CLI and curl. Connect gives the
// same helpers against a server someone else started.
//
// # Passing t
//
// t is passed to each call that does I/O, not captured at Start. A server
// started in a parent test is routinely used inside t.Run subtests, and a
// failure must be reported on the goroutine of the test that is failing.
// Every helper takes a testing.TB, so benchmarks work too, and reports only
// through t.Fatalf and t.Errorf.
//
// # Contexts
//
// Each admin RPC runs under context.Background bounded by a 30-second
// timeout, never under t.Context. t.Context is canceled just before cleanup
// functions run, which would break exactly the calls that belong in cleanup;
// a helper therefore behaves the same in a test body and in a cleanup
// function, and a server that accepts a connection and never answers cannot
// hang the test until go test's own timeout.
//
// # Cleanup order
//
// Cleanups run last-in-first-out. Start registers the server's shutdown
// first, then its Client's transport cleanup, so Dial and any cleanup
// registered after Start run before both: the test's connections close
// before the server stops, and a verifying cleanup runs against a live
// server. Register verifying cleanups after Start. One registered before
// Start runs after the server is gone and fails with a transport error.
//
// # Method names
//
// Every helper that takes a method accepts "/pkg.Svc/M", the form of the
// generated _FullMethodName constants, or "pkg.Svc/M".
//
// # Schemas
//
// Stub, StubYAML and Verify first make the method's service known to the
// server: a service the server does not list but the test binary links is
// registered from its generated code. A schema the binary does not link — a
// dynamicpb schema, a descriptor set read from disk — needs RegisterFiles,
// WithProtoDir or WithDescriptorSet.
//
// RegisterFiles keeps the caller's own descriptor instances, not re-parsed
// copies, and the Client decodes through them: Respond's round-trip guard,
// RespondStatus' details, and Call.Request and Call.Response. Extensions are
// matched by descriptor identity: an extension decoded through one instance of
// a file is invisible through another instance of it, with no error. So build
// messages and extension values from the very FileDescriptor passed to
// RegisterFiles. A different instance of a file that declares extensions is
// refused when registered, and a message built from one is refused when
// decoded into if the decoded message carries a populated extension (one
// without decodes the same through either instance). One case nothing can
// detect: an extension descriptor never passed to RegisterFiles that extends
// a message that was. Build extension values only from extension descriptors
// you registered, or from generated code.
//
// # In-process and container servers
//
// The server falls back to its process's global registries for message, Any
// and extension lookups. In-process, those are the test binary's, so a custom
// status detail or Any payload type the test never registered resolves there
// but fails against a server in a container. Register such types with
// RegisterFiles to behave the same against both. Service lookup has no
// fallback, so automatic registration behaves the same everywhere.
package simulacratest
