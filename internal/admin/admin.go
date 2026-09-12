package admin

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
	"github.com/yinghanhung/simulacra/internal/journal"
	"github.com/yinghanhung/simulacra/internal/schema"
	"github.com/yinghanhung/simulacra/internal/stub"
)

// maxRequestBytes caps both a single Connect message and the whole HTTP request
// stream for every service except SchemaService. The admin plane is
// unauthenticated by design (auth and TLS are M3 non-goals), so without a cap
// any peer can make the server allocate whatever it sends: a probe drove an
// 8 MiB GetServerInfo request to a 200 response.
//
// 4 MiB matches grpc-go's own default receive limit, which is the size callers
// of a gRPC-shaped API already expect.
const maxRequestBytes = 4 << 20

// maxSchemaRequestBytes is SchemaService's cap. RegisterSchemas carries whole
// descriptor sets, and `buf build` images include source info by default and
// grow with the repository (design §8). Raise it deliberately if a real set
// needs more; never remove the cap.
const maxSchemaRequestBytes = 32 << 20

// handshakeTimeout bounds the wait for the second half of the h2c client
// preface. It is a var, not a const, only so tests can shorten it.
var handshakeTimeout = 10 * time.Second

// Deps is everything the control plane needs from the server that hosts it.
type Deps struct {
	Registry *schema.Registry
	Store    *stub.Store
	Journal  *journal.Journal
	Version  string

	// DataAddr and AdminAddr report the bound listener addresses. They are
	// funcs, not strings, because binding happens after the handler is built
	// — a ":0" port is not known until Listen returns — and GetServerInfo
	// must report the real ones.
	DataAddr  func() string
	AdminAddr func() string

	// Shutdown starts server teardown and returns immediately. The server
	// supplies it; the handler must not block on teardown, because the
	// response it is about to write is itself what teardown waits to drain.
	Shutdown func()

	// Stopping is closed when server teardown begins. WatchCalls ends its
	// streams on it, so an open tail does not hold teardown for the whole
	// grace period (design §6).
	Stopping <-chan struct{}
}

func (d Deps) validate() error {
	var missing []string
	if d.Registry == nil {
		missing = append(missing, "Registry")
	}
	if d.Store == nil {
		missing = append(missing, "Store")
	}
	if d.Journal == nil {
		missing = append(missing, "Journal")
	}
	if d.Version == "" {
		missing = append(missing, "Version")
	}
	if d.DataAddr == nil {
		missing = append(missing, "DataAddr")
	}
	if d.AdminAddr == nil {
		missing = append(missing, "AdminAddr")
	}
	if d.Shutdown == nil {
		missing = append(missing, "Shutdown")
	}
	if d.Stopping == nil {
		missing = append(missing, "Stopping")
	}
	if len(missing) > 0 {
		return fmt.Errorf("admin: incomplete Deps, missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

// Install mounts the control plane on srv: the simulacra.admin.v1 ConnectRPC
// routes, GET /healthz, and the h2c wrapping that lets a native gRPC client
// reach them over cleartext HTTP/2.
//
// It takes the *http.Server rather than returning a handler because the h2c
// handler and the *http2.Server driving it must be paired with that specific
// server: http2.ConfigureServer is what registers the graceful-shutdown hook
// that reaches hijacked h2c connections, and without it the admin plane keeps
// serving after the server reports it has stopped. Handing back a bare
// http.Handler would let a caller wire it up in a way that silently leaks
// connections.
func Install(srv *http.Server, deps Deps) error {
	if srv == nil {
		return errors.New("admin: Install requires a non-nil *http.Server")
	}
	if err := deps.validate(); err != nil {
		return err
	}

	mux := http.NewServeMux()
	// Each service caps both one decoded message (connect.WithReadMaxBytes) and
	// its own request stream (http.MaxBytesHandler), with the same limit;
	// connect turns an http.MaxBytesError into RESOURCE_EXHAUSTED. SchemaService
	// alone carries descriptor sets, so it alone gets the larger cap (design §8).
	path, handler := adminv1connect.NewControlServiceHandler(&controlService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewSchemaServiceHandler(&schemaService{deps: deps},
		connect.WithReadMaxBytes(maxSchemaRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxSchemaRequestBytes))
	path, handler = adminv1connect.NewStubServiceHandler(&stubService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewJournalServiceHandler(&journalService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	path, handler = adminv1connect.NewVerifyServiceHandler(&verifyService{deps: deps},
		connect.WithReadMaxBytes(maxRequestBytes))
	mux.Handle(path, http.MaxBytesHandler(handler, maxRequestBytes))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok")
	})

	// routes bounds every request that reaches the mux, at the largest route
	// cap; each route enforces its own cap inside it. It is used both for
	// streams on connections this package upgrades itself and, inside
	// h2c.NewHandler, for streams on connections h2c upgrades — the h2 path
	// never passes through the outer wrapper below, so bounding only there
	// would leave every post-upgrade RPC unlimited.
	routes := http.MaxBytesHandler(mux, maxSchemaRequestBytes)

	h2s := &http2.Server{}
	srv.Handler = &transport{
		h2s: h2s,
		// The outer cap covers HTTP/1.1 requests and, importantly, the h2c
		// upgrade path: x/net's h2cUpgrade does io.ReadAll(r.Body) before any
		// handler runs, so this cap must be at least the largest route cap.
		fallback: http.MaxBytesHandler(h2c.NewHandler(routes, h2s), maxSchemaRequestBytes),
		streams:  routes,
	}
	// Mandatory, not tuning. See the doc comment above.
	if err := http2.ConfigureServer(srv, h2s); err != nil {
		return fmt.Errorf("admin: configuring http/2: %w", err)
	}
	return nil
}

// transport dispatches an incoming admin request. It exists to take the h2c
// prior-knowledge handshake away from x/net's h2c handler, which leaks the
// connection when the client preface never completes.
type transport struct {
	h2s      *http2.Server
	fallback http.Handler // h2c upgrade path and plain HTTP/1.1
	streams  http.Handler // requests on connections upgraded here
}

func (t *transport) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The prior-knowledge probe, per RFC 9113 §3.4 — the same shape x/net's
	// h2c matches on.
	if r.Method == "PRI" && len(r.Header) == 0 && r.URL.Path == "*" && r.Proto == "HTTP/2.0" {
		t.servePriorKnowledge(w, r)
		return
	}
	t.fallback.ServeHTTP(w, r)
}

// servePriorKnowledge completes the cleartext HTTP/2 handshake and serves the
// connection.
//
// This duplicates x/net's h2c.initH2CWithPriorKnowledge because that function
// leaks the connection it hijacks: verified against v0.53.0, its read-error
// path returns without closing conn, and its caller returns before installing
// its own `defer conn.Close()`. Hijacking also clears the deadline
// http.Server derived from ReadHeaderTimeout, so nothing bounds the read
// either. Measured against the unpatched path: a client that sent
// "PRI * HTTP/2.0\r\n\r\n" and then disconnected left the socket and its
// tracking entry alive past 11s, until the server was force-stopped.
//
// The two differences from upstream are the whole point: the close is
// unconditional, and the preface read is bounded.
func (t *transport) servePriorKnowledge(w http.ResponseWriter, r *http.Request) {
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	defer conn.Close()

	if err := conn.SetReadDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	const prefaceTail = "SM\r\n\r\n"
	buf := make([]byte, len(prefaceTail))
	if _, err := io.ReadFull(rw, buf); err != nil {
		return
	}
	if string(buf) != prefaceTail {
		return
	}
	// Clear the handshake bound before handing over: from here the connection
	// is a live h2 session whose lifetime is the server's to manage.
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		return
	}

	baseConfig, _ := r.Context().Value(http.ServerContextKey).(*http.Server)
	t.h2s.ServeConn(bufferedConn(conn, rw), &http2.ServeConnOpts{
		Context:          r.Context(),
		BaseConfig:       baseConfig,
		Handler:          t.streams,
		SawClientPreface: true,
	})
}

// bufferedConn hands the h2 session any bytes the hijack left buffered before
// falling through to the connection. A client normally writes its preface and
// its first SETTINGS frame together, so reading straight from conn would drop
// the frame that was already in net/http's reader.
func bufferedConn(conn net.Conn, rw *bufio.ReadWriter) net.Conn {
	if err := rw.Flush(); err != nil {
		return conn
	}
	if rw.Reader.Buffered() == 0 {
		return conn
	}
	return &prefaceConn{Conn: conn, buffered: rw.Reader}
}

type prefaceConn struct {
	net.Conn
	buffered *bufio.Reader
}

func (c *prefaceConn) Read(p []byte) (int, error) {
	if c.buffered == nil {
		return c.Conn.Read(p)
	}
	n := c.buffered.Buffered()
	if n == 0 {
		c.buffered = nil
		return c.Conn.Read(p)
	}
	if n < len(p) {
		p = p[:n]
	}
	return c.buffered.Read(p)
}
