package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yinghanhung/simulacra/server"
)

// The one compiled binary every binary-level test shares. t.TempDir cannot own
// it — the binary has to outlive the test that first asked for it, which is the
// point of compiling it once — so the directory is package-scoped and TestMain
// removes it.
var (
	binaryOnce sync.Once
	binaryDir  string
	binaryPath string
	binaryErr  error
)

func TestMain(m *testing.M) {
	code := m.Run()
	if binaryDir != "" {
		os.RemoveAll(binaryDir)
	}
	os.Exit(code)
}

// buildBinary compiles cmd/simulacra once per test binary and returns its path.
//
// Two classes of behaviour need the real process rather than an in-process
// command. Signals are one: the failure mode is that no handler is installed,
// and a test that cancels a context instead would pass against exactly that
// bug. Stream discipline is the other: cobra's cmd.Print* family writes to
// OutOrStderr, which falls back to os.Stderr unless something called SetOut,
// and runCmd calls it — so only a real process has a stdout and a stderr that
// can differ (design §4).
func buildBinary(t *testing.T) string {
	t.Helper()
	binaryOnce.Do(func() {
		if binaryDir, binaryErr = os.MkdirTemp("", "simulacra-cli"); binaryErr != nil {
			return
		}
		binaryPath = filepath.Join(binaryDir, "simulacra")
		build := exec.Command("go", "build", "-o", binaryPath, "../../cmd/simulacra")
		if out, err := build.CombinedOutput(); err != nil {
			binaryErr = fmt.Errorf("building cmd/simulacra: %w\n%s", err, out)
		}
	})
	if binaryErr != nil {
		t.Fatal(binaryErr)
	}
	return binaryPath
}

// runBinary runs the built binary to completion with genuinely separate stdout
// and stderr, returning both and the exit status.
func runBinary(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(buildBinary(t), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("running %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return stdout.String(), stderr.String(), code
}

// waitForOutput blocks until the process has written needle to the buffer.
func waitForOutput(t *testing.T, read func() string, needle string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if strings.Contains(read(), needle) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %q; output so far:\n%s", needle, read())
}

// exitStatus runs cmd to completion and returns its exit status.
func exitStatus(t *testing.T, cmd *exec.Cmd) int {
	t.Helper()
	err := cmd.Wait()
	if err == nil {
		return 0
	}
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		t.Fatalf("waiting for the process: %v", err)
	}
	return exit.ExitCode()
}

// exitStatusWithin is exitStatus with a deadline, for the tests whose failure
// mode is a child that never exits at all. An unbounded Wait would surface that
// as the whole package timing out, minutes later and with nothing said about
// which test hung; this names the hang where it happens and reaps the process
// rather than leaving it behind.
func exitStatusWithin(t *testing.T, cmd *exec.Cmd, within time.Duration) int {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return 0
		}
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatalf("waiting for the process: %v", err)
		}
		return exit.ExitCode()
	case <-time.After(within):
		cmd.Process.Kill()
		<-done
		t.Fatalf("the process was still running %v after the signal", within)
		return 0
	}
}

// waitForTailedCall drives calls into srv until one reaches the child's output.
//
// This is the readiness signal the tail signal tests need, and it replaces a
// sleep that guessed at how long the child takes to get there. A delivered call
// is proof rather than a guess: newCallsTailCmd installs its signal context
// before it opens the stream, so a call on stdout means the handler is
// necessarily already in place — which is the precondition those tests rest on
// and exactly what a sleep cannot guarantee under load.
//
// Calls are driven repeatedly, not once. WatchCalls delivers only what is
// recorded after the watch is established, so a single call sent before the
// child subscribed would be missed and the wait would then be for something
// that is never coming.
func waitForTailedCall(t *testing.T, srv *server.Server, cmd *exec.Cmd, out *syncBuffer) {
	t.Helper()
	deadline := time.After(30 * time.Second)
	for !strings.Contains(out.String(), "GetOrder") {
		select {
		case <-deadline:
			cmd.Process.Kill()
			t.Fatalf("tail never received a call; output:\n%s", out.String())
		default:
		}
		recordDataPlaneCall(t, srv, "o-signal")
		time.Sleep(50 * time.Millisecond)
	}
}

// calls tail has no completion criterion: being stopped is how it ends, so a
// signal is success (design §5).
func TestCallsTailExitsZeroOnRealSIGINT(t *testing.T) {
	binary := buildBinary(t)
	srv := startCommandServer(t)

	cmd := exec.Command(binary, "calls", "tail", "--addr", adminAddr(srv))
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForTailedCall(t, srv, cmd, out)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 0 {
		t.Fatalf("calls tail exited %d on SIGINT, want 0; output:\n%s", code, out.String())
	}
}

func TestCallsTailExitsZeroOnRealSIGTERM(t *testing.T) {
	binary := buildBinary(t)
	srv := startCommandServer(t)

	cmd := exec.Command(binary, "calls", "tail", "--addr", adminAddr(srv))
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForTailedCall(t, srv, cmd, out)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 0 {
		t.Fatalf("calls tail exited %d on SIGTERM, want 0; output:\n%s", code, out.String())
	}
}

// Every other client command has a completion criterion it did not reach, so
// an interrupt is an operational failure (design §5).
func TestInterruptedUnaryCommandExitsTwo(t *testing.T) {
	binary := buildBinary(t)
	// A listener that accepts and never responds keeps the RPC in flight,
	// so the signal lands mid-call.
	ln := hangingListener(t)

	cmd := exec.Command(binary, "verify", "--addr", ln,
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1",
		"--timeout", "60s")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 2 {
		t.Fatalf("interrupted verify exited %d, want 2; output:\n%s", code, out.String())
	}
}

// Design §4 puts every command's payload on stdout and its commentary on
// stderr, so `simulacra schema list > services.txt` captures the listing.
//
// This is the only kind of test that can see a violation. runCmd's SetOut makes
// cobra's OutOrStderr — the writer behind cmd.Print, cmd.Printf and
// cmd.Println — resolve to the stdout buffer, so an in-process test records a
// payload on "stdout" whichever stream the real binary would have used. Only a
// separate process with two distinct pipes distinguishes them.
func TestSchemaListWritesTheListingToStdout(t *testing.T) {
	srv := startCommandServer(t)

	stdout, stderr, code := runBinary(t, "schema", "list", "--addr", adminAddr(srv))
	if code != 0 {
		t.Fatalf("schema list exited %d; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("schema list wrote nothing to stdout; stderr=%q", stderr)
	}
	if !strings.Contains(stdout, "shop.v1.OrderService") {
		t.Errorf("stdout = %q, want the service listing", stdout)
	}
	if strings.Contains(stderr, "shop.v1.OrderService") {
		t.Errorf("the listing also reached stderr, so a redirect would double it: %q", stderr)
	}
}

// A failed verify's verdict is the command's output, not a diagnostic about it:
// Execute suppresses the error: prefix precisely because the verdict has
// already been printed (design §5). Printed to stderr it would be invisible to
// the redirect a CI job uses to keep the verdict, while the exit code alone
// says only that something failed.
func TestFailingVerifyWritesItsVerdictToStdout(t *testing.T) {
	srv := startCommandServer(t)
	// No call was ever recorded, so exactly=1 cannot hold.
	stdout, stderr, code := runBinary(t, "verify", "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if code != 1 {
		t.Fatalf("a failed verify exited %d, want 1; stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("a failed verify wrote no verdict to stdout; stderr=%q", stderr)
	}
}

// Regression guard: adding client commands must not change serve's shutdown.
// The first interrupt starts a GRACEFUL stop and says so; only a second one
// forces. A root-level signal context would break this, and neither existing
// shutdown test would catch it — both drive waitAndShutdown* directly.
func TestServeFirstInterruptIsStillGraceful(t *testing.T) {
	binary := buildBinary(t)
	cmd := exec.Command(binary, "serve",
		"--proto", "../../testdata/protos",
		"--listen", "127.0.0.1:0", "--admin", "127.0.0.1:0", "--watch=false")
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	waitForOutput(t, out.String, "data plane listening", 30*time.Second)

	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	waitForOutput(t, out.String, "interrupt again to force", 10*time.Second)
	if strings.Contains(out.String(), "forcing stop") {
		t.Fatalf("the first interrupt forced a stop:\n%s", out.String())
	}
	_ = cmd.Wait()
}

// `stub add -f -` blocks on stdin inside preflight, which runs before the RPC
// phase. The signal context therefore has to be installed before preflight and
// the stdin read has to watch it; otherwise the process sits in the read with
// no handler and dies by its default disposition — shell-visible 130 or 143 —
// instead of the 2 an interrupted non-tail client command owes the caller
// (design §5).
//
// This needs the real process for the same reason the tail tests do: the
// failure mode is the absence of a handler, and an in-process test that
// cancelled a context would pass against exactly that bug. A signal-killed
// child reports ExitCode() -1, so exit 2 is the discriminating observation.
func TestInterruptedStdinReadExitsTwo(t *testing.T) {
	assertStdinInterruptExitsTwo(t, os.Interrupt)
}

func TestSIGTERMDuringStdinReadExitsTwo(t *testing.T) {
	assertStdinInterruptExitsTwo(t, syscall.SIGTERM)
}

func assertStdinInterruptExitsTwo(t *testing.T, sig os.Signal) {
	t.Helper()
	binary := buildBinary(t)
	srv := startCommandServer(t)

	cmd := exec.Command(binary, "stub", "add", "--addr", adminAddr(srv), "-f", "-")
	// A pipe this test holds open and never writes to: the child blocks in the
	// read, which is the only state the bug lives in. Closing it would send
	// EOF and the command would finish on its own.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin pipe: %v", err)
	}
	defer stdin.Close()
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Give the child time to reach the read before signalling.
	time.Sleep(500 * time.Millisecond)
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatus(t, cmd); code != 2 {
		t.Fatalf("stub add -f - interrupted by %v exited %d, want 2; output:\n%s",
			sig, code, out.String())
	}
	// Exit 2 is also what an unreachable server produces, so the wording is
	// what ties this exit to the interrupt rather than to some other failure
	// the command happened to hit first.
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("output does not report an interruption, so exit 2 may be unrelated:\n%s", out.String())
	}
}

// A named `-f <path>` blocks exactly as `-f -` does whenever the path is a
// FIFO, a device node, or a file on a stalled mount, and preflight reads it
// under the same signal context. Reading it synchronously is strictly worse
// than having no handler at all: the handler catches the signal, the context is
// cancelled, and the read goes on blocking — so instead of dying promptly by
// its default disposition the command hangs, and then reports success over
// input it never saw.
//
// A FIFO with its writer held open is that state exactly. Like the stdin tests
// this needs the real process: the failure mode involves signal delivery, which
// an in-process test cannot produce.
func TestInterruptedNamedPathReadExitsTwo(t *testing.T) {
	assertNamedPathInterruptExitsTwo(t, os.Interrupt)
}

func TestSIGTERMDuringNamedPathReadExitsTwo(t *testing.T) {
	assertNamedPathInterruptExitsTwo(t, syscall.SIGTERM)
}

func assertNamedPathInterruptExitsTwo(t *testing.T, sig os.Signal) {
	t.Helper()
	cmd, out, _ := stubAddBlockedOnFIFO(t)
	if err := cmd.Process.Signal(sig); err != nil {
		t.Fatalf("signal: %v", err)
	}
	if code := exitStatusWithin(t, cmd, 30*time.Second); code != 2 {
		t.Fatalf("stub add -f <fifo> interrupted by %v exited %d, want 2; output:\n%s",
			sig, code, out.String())
	}
	// Exit 2 is also what an unreachable server produces, so the wording is
	// what ties this exit to the interrupt rather than to some other failure
	// the command happened to hit first.
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("output does not report an interruption, so exit 2 may be unrelated:\n%s", out.String())
	}
}

// The half that merely unblocking the read would still get wrong.
//
// Closing the writer after the signal lets the read return zero bytes and no
// error, which is indistinguishable from an empty stub file: preflight yields
// no documents, the RPC loop never runs, and a command that took the completed
// read at face value would create nothing, say nothing, and exit 0 for a run
// the operator cut short. Only the already-visible cancellation winning over
// the completed read keeps that from happening, which is why this case is
// tested separately from the hang.
func TestSignalledThenEmptiedNamedPathStillExitsTwo(t *testing.T) {
	cmd, out, writer := stubAddBlockedOnFIFO(t)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatalf("signal: %v", err)
	}
	// The cancellation has to be visible before the read is allowed to finish,
	// or there is nothing here to test: signal delivery is asynchronous, and a
	// read that returns before the handler has run is simply a complete read of
	// an empty file. Waiting for the child to say it was interrupted is that
	// ordering, established rather than assumed.
	//
	// The wait does not insist, and must not: a build with the bug never prints
	// it, and what should report that is the exit status below — exit 0 names
	// the false success — not a timeout here talking about missing output.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(out.String(), "interrupted") && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	// Now let the blocked read finish, empty-handed.
	if err := writer.Close(); err != nil {
		t.Fatalf("closing the FIFO writer: %v", err)
	}
	if code := exitStatusWithin(t, cmd, 30*time.Second); code != 2 {
		t.Fatalf("stub add -f <fifo> signalled and then given EOF exited %d, want 2; output:\n%s",
			code, out.String())
	}
	// The interrupted run must not pass itself off as a successful one that
	// simply had nothing to add.
	if !strings.Contains(out.String(), "interrupted") {
		t.Errorf("the interrupted run reported no interruption:\n%s", out.String())
	}
}

// stubAddBlockedOnFIFO starts `stub add -f <fifo>` on a FIFO and returns once
// the child is in the read, together with the write end the test holds open.
//
// A FIFO is the cheapest way to make a *named* path block the way a pipe on
// stdin does: os.ReadFile's open waits for a writer, and its read then waits
// for bytes that never come. Nothing is ever written, so the child stays in the
// read — the only state the bug lives in.
//
// Opening the write end is also the readiness signal, and it is one rather than
// a guess: a FIFO's two opens rendezvous, so this open returns exactly when the
// child has opened the read end. That is proof the child is inside preflight,
// and therefore that its signal context is already installed — the precondition
// both tests rest on. A sleep cannot establish it, and losing that race does
// not show up as the hang under test: the signal lands before the handler
// exists and the child dies by its default disposition instead, which is a
// different bug's symptom. A cold `exec` of a freshly built binary is enough to
// lose it, as half a second of sleep here originally did.
func stubAddBlockedOnFIFO(t *testing.T) (*exec.Cmd, *syncBuffer, *os.File) {
	t.Helper()
	binary := buildBinary(t)
	srv := startCommandServer(t)

	// t.TempDir removes the FIFO along with the directory, so no named pipe
	// outlives the test.
	fifo := filepath.Join(t.TempDir(), "stubs.yaml")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Fatalf("mkfifo: %v", err)
	}

	cmd := exec.Command(binary, "stub", "add", "--addr", adminAddr(srv), "-f", fifo)
	out := &syncBuffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	// A child still blocked when an assertion fails would outlive the test
	// holding the FIFO open.
	t.Cleanup(func() { cmd.Process.Kill() })

	// The open blocks by design, so it runs on its own goroutine: a child that
	// dies before opening would otherwise wedge the test here instead of
	// failing it. On that path the goroutine stays parked on an open that will
	// never rendezvous, which the failing test then outlives by seconds.
	opened := make(chan *os.File, 1)
	failed := make(chan error, 1)
	go func() {
		writer, err := os.OpenFile(fifo, os.O_WRONLY, 0)
		if err != nil {
			failed <- err
			return
		}
		opened <- writer
	}()
	var writer *os.File
	select {
	case writer = <-opened:
	case err := <-failed:
		t.Fatalf("opening the FIFO write end: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatalf("stub add never opened %s; output:\n%s", fifo, out.String())
	}
	t.Cleanup(func() { writer.Close() })
	return cmd, out, writer
}
