package server

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/yinghanhung/simulacra/internal/admin"
)

// Version is the version string GetServerInfo reports. Nothing in the tree
// carries a version yet and the frozen contract has the field, so it defaults
// to "dev"; M4's release work sets it via ldflags.
var Version = "dev"

// startAdminPlane binds the admin listener and builds the http.Server that
// serves the control plane on it. It runs before either serve goroutine
// starts, so a failure leaves nothing running for the caller to join.
func (s *Server) startAdminPlane(addr string, listen func(network, address string) (net.Listener, error)) error {
	lis, err := listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", addr, err)
	}
	s.adminLis = newTrackedListener(lis)

	srv := &http.Server{
		// A slow-header peer must not be able to pin a connection — and its
		// server goroutine — for the life of the process: this plane is on by
		// default and unauthenticated. ReadHeaderTimeout bounds only the time
		// to read the request head; IdleTimeout bounds only time between
		// requests on a keep-alive connection. Deliberately absent:
		// ReadTimeout and WriteTimeout, which would each cap the duration of
		// a whole request/response and so would break Phase 4b's WatchCalls
		// server-streaming RPC. Do not add them to "complete" this set.
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	deps := admin.Deps{
		Registry: s.reg,
		Store:    s.store,
		Journal:  s.journal,
		Version:  Version,
		// s.lis is always bound by the time this closure could run — Start
		// binds the data listener before calling startAdminPlane — so it
		// needs no nil guard. It stays a func because binding happens before
		// GetServerInfo is ever called, not before this struct literal is
		// built: a ":0" port is not known until Listen returns.
		DataAddr: func() string { return s.lis.Addr().String() },
		// s.adminLis is a *trackedListener, not a plain net.Listener: a nil
		// *trackedListener boxed into a net.Listener interface is a non-nil
		// interface holding a nil pointer, so an `iface == nil` check would
		// not catch it. Checking the concrete pointer here does.
		AdminAddr: func() string {
			if s.adminLis == nil {
				return ""
			}
			return s.adminLis.Addr().String()
		},
		// begin, not a blocking stop: the handler must return so its response
		// can flush, which is exactly what the drain in stopAdminPlane waits
		// for.
		Shutdown: s.begin,
	}
	if err := admin.Install(srv, deps); err != nil {
		_ = s.adminLis.Close()
		s.adminLis = nil
		return err
	}
	s.adminSrv = srv
	s.adminServe = make(chan error, 1)
	return nil
}

// stopAdminPlane is steps 1–3 of the teardown sequence.
//
// http.Server.Shutdown is asymmetric and both halves bite. For the hijacked
// connections a native gRPC client creates it returns instantly and waits for
// nothing; for plain HTTP/1.1 — the Connect path the CLI and Go SDK use — it
// blocks until the request finishes. So it can neither be trusted as a barrier
// nor treated as non-blocking, and the server drains and force-closes itself.
func (s *Server) stopAdminPlane(deadline time.Time) {
	if s.adminSrv == nil {
		return
	}

	// Step 1: stop accepting and start the h2 graceful shutdown (GOAWAY) that
	// http2.ConfigureServer registered. The context is bounded by the budget
	// and canceled by force, because this call blocks on in-flight HTTP/1.1
	// requests; without that, an escalation could not interrupt it and
	// teardown would sit here until the deadline. Its return means "stop
	// accepting and start draining", never "everything is finished".
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	unwatch := make(chan struct{})
	go func() {
		select {
		case <-s.force:
			cancel()
		case <-unwatch:
		}
	}()
	_ = s.adminSrv.Shutdown(ctx)
	close(unwatch)
	cancel()

	// Step 2: the real drain. This is what lets an in-flight ShutdownResponse
	// flush and lets long-lived requests end. Phase 4b's WatchCalls makes it
	// load-bearing: a client tailing calls holds a connection open
	// indefinitely, and only this bound stops it holding teardown open too.
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-s.adminLis.drain():
	case <-s.force:
	case <-timer.C:
	}

	// Step 3: force-close whatever is left. A timeout alone is not enough:
	// http.Server.Shutdown returns without terminating anything, so something
	// must actually close the sockets.
	s.adminLis.closeAll()
}
