// Package simulacratest is a testing.T-integrated Go SDK for Simulacra:
// Start boots an in-process server, and the helpers on Client create stubs,
// reset state, and dial the data plane.
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
package simulacratest
