package simulacratest

import (
	"context"
	"testing"
	"time"

	"github.com/yinghanhung/simulacra/server"
)

// shutdownTimeout bounds Start's cleanup. It sits above the server's
// worst-case teardown (shutdownGrace 5s + dataGraceFloor 1s), so a graceful
// stop finishes inside it; a Shutdown that does hit it escalates to a hard
// stop, so cleanup always returns.
const shutdownTimeout = 10 * time.Second

// Option configures the in-process server Start boots.
type Option func(*server.Options)

// WithProtoDir adds a directory of .proto files as a schema source. Repeatable.
func WithProtoDir(dir string) Option {
	return func(o *server.Options) { o.ProtoDirs = append(o.ProtoDirs, dir) }
}

// WithDescriptorSet adds a serialized FileDescriptorSet file as a schema
// source. Repeatable.
func WithDescriptorSet(path string) Option {
	return func(o *server.Options) { o.DescriptorSetPaths = append(o.DescriptorSetPaths, path) }
}

// WithStubDir loads stub YAML files from dir. Repeatable. The files are read
// once at Start; there is no hot reload.
func WithStubDir(dir string) Option {
	return func(o *server.Options) { o.StubDirs = append(o.StubDirs, dir) }
}

// WithJournalSize sets the recent-call ring capacity. The default is 1024,
// matching `simulacra serve`.
func WithJournalSize(n int) Option {
	return func(o *server.Options) { o.JournalSize = n }
}

// Server is an in-process Simulacra instance. Its embedded Client talks to the
// instance's own admin and data planes over loopback.
type Server struct {
	*Client
	srv *server.Server
}

// Start boots an in-process server on loopback with both planes on ephemeral
// ports, and shuts it down when t finishes.
//
// Cleanups run last-in-first-out, so Start registers shutdown first and the
// Client's transport cleanup second; Dial and any cleanup registered after
// Start run before both. Register verifying cleanups after Start.
func Start(t testing.TB, opts ...Option) *Server {
	t.Helper()
	so := server.Options{
		DataAddr:    "127.0.0.1:0",
		AdminAddr:   "127.0.0.1:0",
		JournalSize: 1024,
	}
	for _, opt := range opts {
		opt(&so)
	}
	srv, err := server.Start(context.Background(), so)
	if err != nil {
		t.Fatalf("simulacratest: Start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			t.Errorf("simulacratest: Shutdown: %v", err)
		}
	})
	return &Server{
		Client: Connect(t, srv.AdminAddr().String(), srv.DataAddr().String()),
		srv:    srv,
	}
}
