package cli

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/yinghanhung/simulacra/gen/simulacra/admin/v1/adminv1connect"
)

const (
	// addrEnv overrides --addr's default, but not an explicit flag.
	addrEnv = "SIMULACRA_ADDR"
	// defaultAddr matches what `serve --admin` binds by default.
	defaultAddr = "localhost:6566"
	// defaultTimeout bounds one unary RPC. A closed port fails fast on its
	// own; this is for a server or proxy that accepts the connection and
	// never returns headers (design §3).
	defaultTimeout = 30 * time.Second
)

// adminClient bundles the five generated service clients against one admin
// plane.
//
// The fields are the generated interfaces rather than concrete types, so a
// test can substitute a decorator around any one of them to inject a failure a
// healthy server cannot produce (design §8). Production always holds the
// generated clients.
type adminClient struct {
	Schema  adminv1connect.SchemaServiceClient
	Stub    adminv1connect.StubServiceClient
	Journal adminv1connect.JournalServiceClient
	Verify  adminv1connect.VerifyServiceClient
	// Control is here for contract completeness — design §2 specifies all
	// five generated clients — and has no consumer yet: no command calls
	// ControlService.
	Control adminv1connect.ControlServiceClient

	baseURL string
	timeout time.Duration
}

// clientFactory builds the client a command talks through. Production
// constructors pass newAdminClient; tests pass a factory that wraps one client
// in a decorator. This mirrors newServeCmdWithListen's existing seam.
type clientFactory func(*cobra.Command) (*adminClient, error)

// clientFlags registers the flags every client command shares.
type clientFlags struct{}

func (clientFlags) register(cmd *cobra.Command) {
	clientFlags{}.registerNoTimeout(cmd)
	cmd.Flags().Duration("timeout", defaultTimeout,
		"deadline for each RPC; 0 disables it")
}

// registerNoTimeout is for `calls tail`, whose stream is unbounded by design
// and which ends on a signal rather than a deadline (design §3).
func (clientFlags) registerNoTimeout(cmd *cobra.Command) {
	cmd.Flags().String("addr", defaultAddr,
		"admin plane address (overridden by the "+addrEnv+" environment variable)")
}

// newAdminClient builds the client from cmd's flags.
func newAdminClient(cmd *cobra.Command) (*adminClient, error) {
	addr, err := cmd.Flags().GetString("addr")
	if err != nil {
		return nil, err
	}
	// Changed() is false when only the environment set the address, which is
	// what keeps an explicit flag winning over the environment.
	if !cmd.Flags().Changed("addr") {
		if env := os.Getenv(addrEnv); env != "" {
			addr = env
		}
	}

	timeout := time.Duration(0)
	if cmd.Flags().Lookup("timeout") != nil {
		if timeout, err = cmd.Flags().GetDuration("timeout"); err != nil {
			return nil, err
		}
		if timeout < 0 {
			return nil, fmt.Errorf("--timeout must not be negative (got %s)", timeout)
		}
	}

	// Connect over HTTP/1.1 — the admin plane speaks it, so no h2c plumbing
	// is needed client-side.
	httpClient := &http.Client{}
	base := baseURL(addr)
	return &adminClient{
		Schema:  adminv1connect.NewSchemaServiceClient(httpClient, base),
		Stub:    adminv1connect.NewStubServiceClient(httpClient, base),
		Journal: adminv1connect.NewJournalServiceClient(httpClient, base),
		Verify:  adminv1connect.NewVerifyServiceClient(httpClient, base),
		Control: adminv1connect.NewControlServiceClient(httpClient, base),
		baseURL: base,
		timeout: timeout,
	}, nil
}

// baseURL turns an address into a base URL, leaving an explicit scheme alone
// so an admin plane behind a proxy or a path prefix is reachable.
func baseURL(addr string) string {
	if strings.HasPrefix(addr, "http://") || strings.HasPrefix(addr, "https://") {
		return addr
	}
	return "http://" + addr
}

// callContext applies the per-RPC deadline. A zero timeout disables it, and
// `calls tail` never registers the flag at all.
func (c *adminClient) callContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, c.timeout)
}

// signalContext derives a command's interrupt-aware context.
//
// Cobra installs none: ExecuteC substitutes context.Background() when no
// context was supplied, so without this a client command dies on SIGINT by the
// process default — status 130 — rather than the exit code the contract
// promises.
//
// This is deliberately per command and never at the root. A root-level signal
// context would cancel serve's command context on the first interrupt, and
// waitAndShutdownContextStop forces immediately on a cancelled context, which
// would collapse serve's two-stage "interrupt again to force" behaviour.
func signalContext(cmd *cobra.Command) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
}

// rpcError converts a failed RPC into the error the command returns. When the
// command's own signal context has ended, the failure is an interruption
// whatever the transport reported: a cancelled call surfaces as a transport
// error that says nothing about why.
func rpcError(sigCtx context.Context, err error) error {
	if sigCtx.Err() != nil {
		return errInterrupted
	}
	return err
}
