// Package server is the public wiring facade for a Simulacra instance. It
// builds the schema registry, stub store, journal, and data-plane gRPC server
// from a single Options value, binds the data listener, and serves in the
// background. The CLI, the Go SDK's in-process mode, and tests all start a
// server the same way.
//
// The admin control plane (Options.AdminAddr / Server.AdminAddr) is added in a
// later milestone phase; Options and Server are designed to grow it without
// breaking callers.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"

	"google.golang.org/grpc"

	"github.com/yinghanhung/simulacra/internal/dataplane"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// Reporter receives asynchronous progress messages — currently stub hot-reload
// results and watch errors. The CLI supplies an implementation backed by the
// command's output streams; embedders that want silence leave it nil.
//
// Calls run on the watcher's own goroutines, which Wait and Shutdown join, so
// an implementation that blocks indefinitely blocks shutdown by that much.
// Implementations must return; buffer or drop instead of waiting on a consumer.
type Reporter interface {
	Printf(format string, args ...any)
	PrintErrln(args ...any)
}

// Options configures a server. At least one schema source (ProtoDirs or
// DescriptorSetPaths) is required: a server with no schema could answer
// nothing. JournalSize must be greater than zero.
type Options struct {
	ProtoDirs          []string // directories of .proto files (each is an import root)
	DescriptorSetPaths []string // serialized FileDescriptorSet files
	StubDirs           []string // directories of stub YAML files
	DataAddr           string   // data-plane listen address, e.g. ":6565"
	JournalSize        int      // recent-call ring capacity (must be > 0)
	Watch              bool     // watch StubDirs and hot-reload

	// Listen overrides the listener factory so tests can inject fakes.
	// Nil uses net.Listen.
	Listen func(network, address string) (net.Listener, error)
	// Reporter receives hot-reload progress. Nil discards it.
	Reporter Reporter
}

// Server is a running Simulacra instance.
type Server struct {
	reg     *schema.Registry
	store   *stub.Store
	journal *journal.Journal
	data    *dataplane.Server
	lis     net.Listener

	serveResult chan error
	waitOnce    sync.Once
	waitErr     error

	watcher watcherRun
}

// Start builds and starts a server, returning once the data listener is bound.
// The returned Server serves in the background until GracefulStop, Stop, or
// Shutdown is called.
//
// ctx scopes startup only: it aborts schema loading and the watcher's initial
// attach, and canceling it after Start returns has no effect. The Server owns
// everything it launched — including the Watch: true stub watcher — so a caller
// can pass a startup timeout without silently disarming hot reload underneath a
// data plane that is still serving.
func Start(ctx context.Context, opts Options) (*Server, error) {
	if opts.JournalSize <= 0 {
		return nil, fmt.Errorf("journal size must be greater than zero (got %d)", opts.JournalSize)
	}
	listen := opts.Listen
	if listen == nil {
		listen = net.Listen
	}
	reporter := opts.Reporter
	if reporter == nil {
		reporter = discardReporter{}
	}

	reg, err := buildRegistry(ctx, opts.ProtoDirs, opts.DescriptorSetPaths)
	if err != nil {
		return nil, err
	}
	stubs, err := loadStubs(reg, opts.StubDirs, reporter)
	if err != nil {
		return nil, err
	}
	store := stub.NewStore(stubs)
	jrnl := journal.New(opts.JournalSize)
	data, err := dataplane.New(reg, store, jrnl)
	if err != nil {
		return nil, err
	}

	s := &Server{
		reg:         reg,
		store:       store,
		journal:     jrnl,
		data:        data,
		serveResult: make(chan error, 1),
	}

	if opts.Watch && len(opts.StubDirs) > 0 {
		watcher, err := s.launchStubWatcher(ctx, opts.StubDirs, reporter)
		if err != nil {
			return nil, err
		}
		s.watcher = watcher
	}

	lis, err := listen("tcp", opts.DataAddr)
	if err != nil {
		s.watcher.stop()
		return nil, fmt.Errorf("listening on %s: %w", opts.DataAddr, err)
	}
	s.lis = lis

	go func() { s.serveResult <- normalizeServeErr(s.data.Serve(lis)) }()
	return s, nil
}

// normalizeServeErr maps grpc's "server has been stopped" sentinel to nil.
// Serve returns it when Stop or GracefulStop wins the race against the serve
// goroutine's first scheduling (grpc-go returns ErrServerStopped immediately
// when its listener set was already torn down). That is a clean stop, not a
// failure, so Wait must not surface it. The conformance harness already
// applies the same normalization (conformance/harness_test.go, waitForServe).
func normalizeServeErr(err error) error {
	if errors.Is(err, grpc.ErrServerStopped) {
		return nil
	}
	return err
}

// DataAddr is the bound data-plane listener address.
func (s *Server) DataAddr() net.Addr { return s.lis.Addr() }

// ServiceCount is the number of registered services (mocked services + health).
func (s *Server) ServiceCount() int { return len(s.reg.Services()) }

// StubCount is the number of currently loaded stubs. It is read from the store
// itself, so it changes exactly when a hot reload swaps the stubs in — never
// later than the stubs those calls are already being answered with.
func (s *Server) StubCount() int { return s.store.Len() }

// Wait blocks until the data-plane server stops, returning Serve's error
// (nil after a clean GracefulStop or Stop). Safe to call from one goroutine.
//
// Before returning it also joins the stub watcher and any reload already in
// flight, so once Wait returns nothing the server started is still touching the
// store or the Reporter. A Reporter that blocks delays that join (see Reporter).
func (s *Server) Wait() error {
	s.waitOnce.Do(func() {
		s.waitErr = <-s.serveResult
		s.watcher.stop()
	})
	return s.waitErr
}

// GracefulStop stops the data plane, waiting for in-flight RPCs (including open
// streams); it can block indefinitely. Callers that need a bound fall back to
// Stop.
func (s *Server) GracefulStop() {
	s.watcher.cancelNow()
	s.data.GracefulStop()
}

// Stop aborts the data plane immediately.
func (s *Server) Stop() {
	s.watcher.cancelNow()
	s.data.Stop()
}

// Shutdown gracefully stops the server, forcing a hard stop if ctx is done
// before the graceful stop completes, and returns once fully stopped.
//
// ctx bounds the graceful phase, not the final join: Shutdown ends with Wait,
// which waits out an in-flight stub reload and its Reporter calls.
func (s *Server) Shutdown(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		s.GracefulStop()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		s.Stop()
		<-done
	}
	return s.Wait()
}

func (s *Server) launchStubWatcher(ctx context.Context, dirs []string, reporter Reporter) (watcherRun, error) {
	reconcile := func(ctx context.Context, announce bool) error {
		_, err := reconcileStubDirs(ctx, reporter, s.reg, s.store, dirs, announce)
		return err
	}
	return startStubWatcher(ctx, dirs, reporter,
		func(ctx context.Context) { _ = reconcile(ctx, true) },
		func(ctx context.Context) error { return reconcile(ctx, false) },
		stub.WatchWithOptions)
}

func buildRegistry(ctx context.Context, protoDirs, descriptorSets []string) (*schema.Registry, error) {
	if len(protoDirs) == 0 && len(descriptorSets) == 0 {
		return nil, errors.New("at least one schema source is required (a proto directory or a descriptor set)")
	}
	reg := schema.NewRegistry()
	for _, d := range protoDirs {
		if err := reg.AddProtoDir(ctx, d); err != nil {
			return nil, err
		}
	}
	for _, f := range descriptorSets {
		if err := reg.AddDescriptorSetFile(f); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func loadStubs(reg *schema.Registry, dirs []string, reporter Reporter) ([]*stub.Compiled, error) {
	stubs, errs := stub.LoadDirs(reg, dirs)
	for _, e := range errs {
		reporter.PrintErrln("stub error:", e)
	}
	if len(errs) > 0 {
		return nil, fmt.Errorf("%d invalid stub(s)", len(errs))
	}
	return stubs, nil
}

type discardReporter struct{}

func (discardReporter) Printf(string, ...any) {}
func (discardReporter) PrintErrln(...any)     {}
