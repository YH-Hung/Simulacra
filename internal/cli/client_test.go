package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	adminv1 "github.com/yinghanhung/simulacra/gen/simulacra/admin/v1"
)

// probeCmd is a command carrying the client flags, used to drive
// newAdminClient the way a real command does.
func probeCmd(t *testing.T, capture func(*adminClient, *cobra.Command) error, args ...string) error {
	t.Helper()
	flags := &clientFlags{}
	cmd := &cobra.Command{
		Use:         "probe",
		Annotations: clientAnnotations(),
		RunE: func(c *cobra.Command, _ []string) error {
			client, err := newAdminClient(c)
			if err != nil {
				return err
			}
			return capture(client, c)
		},
	}
	flags.register(cmd)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	_, _, err := runCmd(t, cmd, args...)
	return err
}

// Precedence is flag > SIMULACRA_ADDR > default (design §3).
func TestAddrResolutionPrecedence(t *testing.T) {
	t.Run("default when neither is set", func(t *testing.T) {
		var got string
		if err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://localhost:6566" {
			t.Fatalf("baseURL = %q, want http://localhost:6566", got)
		}
	})

	t.Run("env wins over the default", func(t *testing.T) {
		t.Setenv("SIMULACRA_ADDR", "127.0.0.1:7000")
		var got string
		if err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}); err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://127.0.0.1:7000" {
			t.Fatalf("baseURL = %q, want the env value", got)
		}
	})

	t.Run("flag wins over env", func(t *testing.T) {
		t.Setenv("SIMULACRA_ADDR", "127.0.0.1:7000")
		var got string
		err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}, "--addr", "127.0.0.1:8000")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "http://127.0.0.1:8000" {
			t.Fatalf("baseURL = %q, want the flag value", got)
		}
	})

	t.Run("an explicit scheme is kept verbatim", func(t *testing.T) {
		var got string
		err := probeCmd(t, func(c *adminClient, _ *cobra.Command) error {
			got = c.baseURL
			return nil
		}, "--addr", "https://admin.example:443/simulacra")
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if got != "https://admin.example:443/simulacra" {
			t.Fatalf("baseURL = %q, want the URL unchanged", got)
		}
	})
}

// Cobra accepts a negative duration silently, and a negative deadline expires
// every context, so every RPC would fail instantly and the user would be
// debugging the server (design §3).
func TestNegativeTimeoutIsRejected(t *testing.T) {
	err := probeCmd(t, func(*adminClient, *cobra.Command) error { return nil },
		"--timeout", "-1s")
	if err == nil {
		t.Fatal("a negative --timeout was accepted")
	}
	if !strings.Contains(err.Error(), "timeout") {
		t.Fatalf("error = %v, want it to name --timeout", err)
	}
}

func TestZeroTimeoutDisablesTheDeadline(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		if _, ok := ctx.Deadline(); ok {
			t.Error("--timeout 0 still set a deadline")
		}
		return nil
	}, "--timeout", "0")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}

func TestTimeoutAppliesADeadline(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		deadline, ok := ctx.Deadline()
		if !ok {
			t.Fatal("no deadline applied")
		}
		if remaining := time.Until(deadline); remaining > 6*time.Second {
			t.Errorf("deadline is %v away, want about 5s", remaining)
		}
		return nil
	}, "--timeout", "5s")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}

// A server that accepts the connection and never responds is exactly what the
// deadline exists for: a closed port already fails fast, this does not.
func TestUnaryCallAgainstAHangingServerHitsTheDeadline(t *testing.T) {
	addr := hangingListener(t)

	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		_, err := c.Stub.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
		return err
	}, "--addr", addr, "--timeout", "500ms")
	if err == nil {
		t.Fatal("the call returned against a server that never responded")
	}
	if code := connect.CodeOf(err); code != connect.CodeDeadlineExceeded {
		t.Fatalf("code = %v, want deadline_exceeded", code)
	}
}

func TestUnreachableAddressIsUnavailable(t *testing.T) {
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		_, err := c.Stub.ListStubs(ctx, connect.NewRequest(&adminv1.ListStubsRequest{}))
		return err
	}, "--addr", "127.0.0.1:1")
	if code := connect.CodeOf(err); code != connect.CodeUnavailable {
		t.Fatalf("code = %v, want unavailable", code)
	}
}

// A signal that arrives mid-call is reported as an interruption, not as
// whatever the transport happened to surface (design §5).
func TestRPCErrorReportsInterruptionWhenSignalled(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	got := rpcError(cancelled, connect.NewError(connect.CodeUnavailable, errors.New("transport closed")))
	if !errors.Is(got, errInterrupted) {
		t.Fatalf("rpcError = %v, want errInterrupted", got)
	}

	live := context.Background()
	transport := connect.NewError(connect.CodeUnavailable, errors.New("transport closed"))
	if got := rpcError(live, transport); !errors.Is(got, transport) {
		t.Fatalf("rpcError = %v, want the transport error unchanged", got)
	}
}

// The client reaches a real server, proving the Connect-over-HTTP/1.1 path.
func TestClientReachesTheAdminPlane(t *testing.T) {
	srv := startCommandServer(t)
	err := probeCmd(t, func(c *adminClient, cmd *cobra.Command) error {
		ctx, cancel := c.callContext(cmd.Context())
		defer cancel()
		resp, err := c.Schema.ListServices(ctx, connect.NewRequest(&adminv1.ListServicesRequest{}))
		if err != nil {
			return err
		}
		if len(resp.Msg.GetServices()) == 0 {
			t.Error("no services reported by a server started with testdata protos")
		}
		return nil
	}, "--addr", adminAddr(srv))
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
}
