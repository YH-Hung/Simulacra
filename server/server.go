// Package server is the public wiring facade for a Simulacra instance. It
// builds the schema registry, stub store, journal, and data-plane gRPC server
// from a single Options value, binds the data listener, and serves in the
// background. The CLI, the Go SDK's in-process mode, and tests all start a
// server the same way.
//
// When Options.AdminAddr is set, it also binds the admin control plane
// listener and serves it alongside the data plane; the two are started,
// tracked, and stopped together as one Server.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

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
// DescriptorSetPaths) is required unless AdminAddr is set: a server with no
// schema and no control plane could answer nothing. JournalSize must be
// greater than zero.
type Options struct {
	ProtoDirs          []string // directories of .proto files (each is an import root)
	DescriptorSetPaths []string // serialized FileDescriptorSet files
	StubDirs           []string // directories of stub YAML files
	DataAddr           string   // data-plane listen address, e.g. ":6565"
	AdminAddr          string   // admin-plane listen address, e.g. ":6566"; empty disables it
	JournalSize        int      // recent-call ring capacity (must be > 0)
	Watch              bool     // watch StubDirs and hot-reload

	// Listen overrides the listener factory so tests can inject fakes.
	// Nil uses net.Listen.
	Listen func(network, address string) (net.Listener, error)
	// Reporter receives hot-reload progress. Nil discards it.
	Reporter Reporter
}

// shutdownGrace bounds runTeardown: the admin drain and the data plane's
// graceful phase share one budget, so that portion of teardown time is
// predictable rather than the sum of independent timeouts. The admin drain
// has first claim on that budget — it runs first in runTeardown — and can
// consume the whole thing: a single stuck HTTP/1.1 request, or a WatchCalls
// Send blocked on a client that stopped reading, is enough. Regardless of
// how much the admin drain consumed, the data plane is still guaranteed
// dataGraceFloor of its own graceful window (see that constant's doc
// comment), so worst-case teardown is shutdownGrace + dataGraceFloor, not
// shutdownGrace. The final joins in supervise — the serve goroutines and the
// watcher — happen after runTeardown returns and are not bounded by either
// budget; watcher.stop() in particular is documented as blocking for as long
// as an in-flight reload takes.
//
// It is a constant rather than an option because ShutdownRequest is empty and
// frozen: the policy is a decision, not a parameter. Unbounded graceful waits
// on open bidi streams — exactly what a failed test leaves behind — after the
// client has already been told OK, so it cannot learn it is hung. Adding a
// timeout field to the proto later is non-breaking.
const shutdownGrace = 5 * time.Second

// dataGraceFloor is the graceful window the data plane is guaranteed even when
// the admin drain consumed the whole budget. Without it, an admin drain that
// runs to the deadline — a stuck HTTP/1.1 request, or a WatchCalls Send
// blocked on a client that stopped reading — would hand stopDataPlane an
// already-expired deadline and hard-kill in-flight RPCs with no graceful phase
// at all. Worst-case teardown is therefore shutdownGrace + dataGraceFloor.
const dataGraceFloor = time.Second

// Server is a running Simulacra instance.
type Server struct {
	reg     *schema.Registry
	store   *stub.Store
	journal *journal.Journal
	data    *dataplane.Server
	lis     net.Listener

	adminSrv *http.Server
	adminLis *trackedListener

	dataServe  chan error
	adminServe chan error // nil while the admin plane is disabled

	// Starting teardown and escalating to force are two independent
	// idempotent signals, never one sync.Once: Once.Do blocks concurrent
	// callers until the first invocation returns, so a single Once would make
	// Stop wait out the GracefulStop it is meant to cut short.
	stopOnce  sync.Once
	stopping  chan struct{}
	forceOnce sync.Once
	force     chan struct{}

	// teardown is closed by supervise once everything has stopped. waitErr is
	// written before that close and read only after it, so every waiter sees
	// the final value.
	teardown chan struct{}
	waitErr  error

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
	// A schema source is required only when the admin plane is off. With it
	// on, the server may boot empty and take schemas over the control plane
	// (RegisterSchemas) — the container/SDK path, where the server
	// must be healthy before any schema exists. With it off, that server could
	// never answer anything, so it still refuses to start.
	if opts.AdminAddr == "" && len(opts.ProtoDirs) == 0 && len(opts.DescriptorSetPaths) == 0 {
		return nil, errors.New("at least one schema source is required (a proto directory or a descriptor set) unless the admin plane is enabled")
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
	store := stub.NewStore()
	if _, err := store.ReplaceOrigin(stub.OriginFile, stubs); err != nil {
		return nil, err
	}
	jrnl := journal.New(opts.JournalSize)
	data, err := dataplane.New(reg, store, jrnl)
	if err != nil {
		return nil, err
	}

	s := &Server{
		reg:       reg,
		store:     store,
		journal:   jrnl,
		data:      data,
		dataServe: make(chan error, 1),
		stopping:  make(chan struct{}),
		force:     make(chan struct{}),
		teardown:  make(chan struct{}),
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

	if opts.AdminAddr != "" {
		// Fatal, matching the data plane's rule: a server asked for a control
		// plane that cannot provide one must not start half-working. Nothing
		// is serving yet, so the listener and the watcher are all there is to
		// undo.
		if err := s.startAdminPlane(opts.AdminAddr, listen); err != nil {
			_ = lis.Close()
			s.watcher.stop()
			return nil, err
		}
		go func() { s.adminServe <- normalizeServeErr(s.adminSrv.Serve(s.adminLis)) }()
	}

	go func() { s.dataServe <- normalizeServeErr(s.data.Serve(lis)) }()
	go s.supervise()
	return s, nil
}

// normalizeServeErr maps the clean-stop sentinels of both planes to nil.
// grpc's Serve returns ErrServerStopped when a stop wins the race against the
// serve goroutine's first scheduling, and http.Server.Serve returns
// ErrServerClosed after Shutdown. Neither is a failure, so Wait must not
// surface them. The conformance harness already applies the grpc half
// (conformance/harness_test.go, waitForServe).
func normalizeServeErr(err error) error {
	if errors.Is(err, grpc.ErrServerStopped) || errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// begin starts teardown exactly once and returns immediately: the sequence
// runs on the supervisor's goroutine. That is what lets the Shutdown RPC
// trigger a teardown which is itself waiting for that RPC's response to flush.
func (s *Server) begin() { s.stopOnce.Do(func() { close(s.stopping) }) }

// escalate abandons the graceful phase exactly once.
func (s *Server) escalate() { s.forceOnce.Do(func() { close(s.force) }) }

// DataAddr is the bound data-plane listener address.
func (s *Server) DataAddr() net.Addr { return s.lis.Addr() }

// AdminAddr is the bound admin-plane listener address, or nil when the admin
// plane is disabled.
func (s *Server) AdminAddr() net.Addr {
	if s.adminLis == nil {
		return nil
	}
	return s.adminLis.Addr()
}

// ServiceCount is the number of registered services (mocked services + health).
func (s *Server) ServiceCount() int { return len(s.reg.Services()) }

// StubCount is the number of currently loaded stubs. It is read from the store
// itself, so it changes exactly when a hot reload swaps the stubs in — never
// later than the stubs those calls are already being answered with.
func (s *Server) StubCount() int { return s.store.Len() }

// GracefulStop stops the whole server — every plane, plus the stub watcher —
// waiting for in-flight work up to shutdownGrace and then forcing.
func (s *Server) GracefulStop() { s.begin(); <-s.teardown }

// Stop stops the whole server, abandoning the graceful phase immediately. It
// returns promptly relative to the graceful phase it cuts short — which is
// what the CLI's second-signal path depends on — but not unconditionally: it
// still blocks on supervise's post-teardown joins, including s.watcher.stop(),
// which is documented as unbounded. See Wait's join paragraph for the full
// list.
func (s *Server) Stop() { s.begin(); s.escalate(); <-s.teardown }

// Wait blocks until the server has fully stopped, returning the error that
// stopped it (nil after a clean GracefulStop, Stop, or Shutdown). Safe to call
// from any number of goroutines.
//
// Before returning it joins both serve goroutines and the stub watcher — plus
// any reload already in flight — so once Wait returns nothing the server
// started is still touching the store or the Reporter. A Reporter that blocks
// delays that join (see Reporter).
func (s *Server) Wait() error { <-s.teardown; return s.waitErr }

// Shutdown stops the whole server, forcing a hard stop if ctx is done before
// the graceful phase completes, and returns once fully stopped.
func (s *Server) Shutdown(ctx context.Context) error {
	s.begin()
	select {
	case <-s.teardown:
	case <-ctx.Done():
		s.escalate()
		<-s.teardown
	}
	return s.waitErr
}

// supervise owns teardown. It waits for either an explicit stop or the first
// Serve loop to exit on its own, runs the teardown sequence, joins everything
// the server started, and publishes the error Wait reports.
//
// Watching both serve loops is what keeps a half-dead server from going
// unnoticed: if one plane's Serve exits unexpectedly — a broken listener, an
// unrecoverable accept error — the sibling keeps running and Wait would
// otherwise block forever on a goroutine that never exits. Wait returns the
// originating error, so the caller learns why the server died rather than
// seeing a nil from the plane that was merely told to stop.
func (s *Server) supervise() {
	var originErr, dataErr, adminErr error
	dataDone := false
	adminDone := s.adminServe == nil

	select {
	case dataErr = <-s.dataServe:
		dataDone = true
		originErr = dataErr
	case adminErr = <-s.adminServe: // a nil channel blocks forever, as intended
		adminDone = true
		originErr = adminErr
	case <-s.stopping:
	}
	s.begin()
	s.runTeardown()
	if !dataDone {
		dataErr = <-s.dataServe
	}
	if !adminDone {
		adminErr = <-s.adminServe
	}
	s.watcher.stop()

	s.waitErr = firstErr(originErr, dataErr, adminErr)
	close(s.teardown)
}

// runTeardown is the one bounded sequence every stop path reaches. Every
// blocking step inside it is force-responsive, so an escalation short-circuits
// whichever one is in flight.
func (s *Server) runTeardown() {
	deadline := time.Now().Add(shutdownGrace)
	s.watcher.cancelNow()
	s.stopAdminPlane(deadline)

	// The data plane is guaranteed dataGraceFloor regardless of how much of
	// shutdownGrace the admin drain above just consumed — see dataGraceFloor's
	// doc comment. Only extend, never shorten: stopAdminPlane may have
	// returned well inside the budget, and the data plane keeps whatever of
	// shutdownGrace is still left in that case.
	dataDeadline := deadline
	if floor := time.Now().Add(dataGraceFloor); floor.After(dataDeadline) {
		dataDeadline = floor
	}
	s.stopDataPlane(dataDeadline)
}

// stopDataPlane runs grpc's graceful stop, abandoning it for Stop when the
// budget runs out or an escalation arrives. grpc-go's Stop is what unblocks a
// GracefulStop already in flight; the CLI has driven the pair this way since
// M1 (internal/cli/shutdown.go).
func (s *Server) stopDataPlane(deadline time.Time) {
	done := make(chan struct{})
	go func() {
		s.data.GracefulStop()
		close(done)
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-done:
		return
	case <-s.force:
	case <-timer.C:
	}
	s.data.Stop()
	<-done
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
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
