package cli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseTimesGrammar(t *testing.T) {
	t.Run("exactly", func(t *testing.T) {
		times, err := parseTimes([]string{"exactly=2"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetExactly() != 2 {
			t.Fatalf("exactly = %d, want 2", times.GetExactly())
		}
	})

	t.Run("never", func(t *testing.T) {
		times, err := parseTimes([]string{"never"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if !times.GetNever() {
			t.Fatal("never was not set")
		}
	})

	t.Run("a comma-separated range", func(t *testing.T) {
		times, err := parseTimes([]string{"at-least=1,at-most=3"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetAtLeast() != 1 || times.GetAtMost() != 3 {
			t.Fatalf("range = [%d,%d], want [1,3]", times.GetAtLeast(), times.GetAtMost())
		}
	})

	t.Run("repeated flags accumulate", func(t *testing.T) {
		times, err := parseTimes([]string{"at-least=1", "at-most=3"})
		if err != nil {
			t.Fatalf("parseTimes: %v", err)
		}
		if times.GetAtLeast() != 1 || times.GetAtMost() != 3 {
			t.Fatalf("range = [%d,%d], want [1,3]", times.GetAtLeast(), times.GetAtMost())
		}
	})

	t.Run("an unknown key is rejected", func(t *testing.T) {
		if _, err := parseTimes([]string{"at-leest=1"}); err == nil {
			t.Fatal("an unknown key was accepted")
		}
	})

	// An unknown key carrying no value is still an unknown key. Deciding the
	// has-a-value question first answers "--times bogus" with "needs a value,
	// e.g. bogus=2" — advice recommending a spelling that is itself rejected.
	t.Run("an unknown key with no value is reported as unknown", func(t *testing.T) {
		_, err := parseTimes([]string{"bogus"})
		if err == nil {
			t.Fatal("an unknown key was accepted")
		}
		if !strings.Contains(err.Error(), "not a known key") {
			t.Fatalf("error = %v, want it to say the key is unknown", err)
		}
		if strings.Contains(err.Error(), "bogus=2") {
			t.Fatalf("error = %v, recommends an invalid spelling", err)
		}
	})

	t.Run("a bare = is rejected without a strconv diagnostic", func(t *testing.T) {
		_, err := parseTimes([]string{"="})
		if err == nil {
			t.Fatal("a bare = was accepted")
		}
		if strings.Contains(err.Error(), "strconv") || strings.Contains(err.Error(), "invalid syntax") {
			t.Fatalf("error = %v, want a message about the flag rather than the parser", err)
		}
	})

	t.Run("a key with an empty value names the key", func(t *testing.T) {
		_, err := parseTimes([]string{"exactly="})
		if err == nil {
			t.Fatal("exactly= was accepted")
		}
		if !strings.Contains(err.Error(), "exactly") {
			t.Fatalf("error = %v, want it to name the key", err)
		}
		if strings.Contains(err.Error(), "strconv") || strings.Contains(err.Error(), "invalid syntax") {
			t.Fatalf("error = %v, want a message about the flag rather than the parser", err)
		}
	})

	t.Run("a non-integer is rejected", func(t *testing.T) {
		if _, err := parseTimes([]string{"exactly=two"}); err == nil {
			t.Fatal("a non-integer was accepted")
		}
	})

	// A key given twice is an error naming it, not last-one-wins: a caller
	// assembling flags from two places would otherwise get a verdict it did
	// not ask for, silently (design §6.3).
	t.Run("a repeated key is rejected", func(t *testing.T) {
		_, err := parseTimes([]string{"exactly=1", "exactly=2"})
		if err == nil {
			t.Fatal("a repeated key was accepted")
		}
		if !strings.Contains(err.Error(), "exactly") {
			t.Fatalf("error = %v, want it to name the repeated key", err)
		}
	})
}

// The wire fields are int32. Atoi followed by a conversion wraps silently,
// turning 2147483648 into -2147483648 and sending the server a different
// assertion under the user's name (design §6.3).
func TestParseTimesRejectsValuesOutsideInt32(t *testing.T) {
	for _, spec := range []string{
		"exactly=2147483648",
		"exactly=-2147483649",
		"at-least=2147483648",
		"at-most=9999999999999",
	} {
		t.Run(spec, func(t *testing.T) {
			if _, err := parseTimes([]string{spec}); err == nil {
				t.Fatalf("%s was accepted; it does not fit int32", spec)
			}
		})
	}

	// The boundaries themselves are valid.
	if _, err := parseTimes([]string{"exactly=2147483647"}); err != nil {
		t.Fatalf("the int32 maximum was rejected: %v", err)
	}
}

func TestVerifyRequiresTimes(t *testing.T) {
	_, _, err := runCmd(t, newVerifyCmd(), "--addr", "127.0.0.1:1",
		"--method", "shop.v1.OrderService/GetOrder")
	if err == nil {
		t.Fatal("verify ran without --times")
	}
	if !strings.Contains(err.Error(), "--times") {
		t.Fatalf("error = %v, want it to name --times", err)
	}
}

func TestVerifyPassExitsZero(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if err != nil {
		t.Fatalf("verify of a call that happened: %v", err)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("verify printed no verdict")
	}
}

// A failed assertion is the command's output, not a diagnostic about it: it
// maps to exit 1 and carries no error: prefix (design §5).
func TestVerifyFailureIsAnAssertionError(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	cmd := newVerifyCmd()
	stdout, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=5")
	if err == nil {
		t.Fatal("verify passed an assertion that should have failed")
	}
	if !errors.Is(err, errAssertionFailed) {
		t.Fatalf("error = %v, want errAssertionFailed", err)
	}
	if got := exitCode(cmd, err); got != 1 {
		t.Fatalf("exitCode = %d, want 1", got)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Error("a failed verify printed no verdict")
	}
}

// The nearest-miss text is the shared diagnostic engine's, reproduced verbatim.
func TestVerifyPrintsNearestMissOnFailure(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	matcher := filepath.Join(t.TempDir(), "match.yaml")
	if err := os.WriteFile(matcher, []byte("message:\n  order_id: { eq: o-999 }\n"), 0o600); err != nil {
		t.Fatalf("write matcher: %v", err)
	}
	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder",
		"--match-file", matcher, "--times", "exactly=1")
	if err == nil {
		t.Fatal("verify matched a call it should not have")
	}
	if !strings.Contains(stdout, "o-1") {
		t.Errorf("stdout = %q, want the nearest-miss detail naming the actual value", stdout)
	}
}

// An operational failure is exit 2, not 1: the assertion never ran.
func TestVerifyAgainstAnUnreachableServerIsOperational(t *testing.T) {
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", "127.0.0.1:1",
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if err == nil {
		t.Fatal("verify succeeded against a closed port")
	}
	if errors.Is(err, errAssertionFailed) {
		t.Fatal("an unreachable server was reported as a failed assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

// Combination validation belongs to the server; the CLI surfaces its
// INVALID_ARGUMENT rather than duplicating the rules (design §6.3).
func TestVerifyLeavesCombinationValidationToTheServer(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "never,exactly=1")
	if err == nil {
		t.Fatal("never combined with exactly was accepted")
	}
	if errors.Is(err, errAssertionFailed) {
		t.Fatal("an invalid times combination was reported as a failed assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

func TestVerifyJSON(t *testing.T) {
	srv := startCommandServer(t)
	recordDataPlaneCall(t, srv, "o-1")

	stdout, _, err := runCmd(t, newVerifyCmd(), "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1",
		"--output", "json")
	if err != nil {
		t.Fatalf("verify --output json: %v", err)
	}
	fields := decodeJSON(t, stdout)
	if fields["passed"] != true {
		t.Fatalf("passed = %v, want true: %s", fields["passed"], stdout)
	}
}

// A mistyped method is NOT_FOUND server-side, not a silent pass — 4b resolves
// the method even for an empty matcher.
func TestVerifyOfAMistypedMethodIsOperational(t *testing.T) {
	srv := startCommandServer(t)
	cmd := newVerifyCmd()
	_, _, err := runCmd(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrdr", "--times", "never")
	if err == nil {
		t.Fatal("a mistyped method passed a never assertion")
	}
	if got := exitCode(cmd, err); got != 2 {
		t.Fatalf("exitCode = %d, want 2", got)
	}
}

// Exit 1 means "the assertion ran and failed", and a CI job reads it as the
// verdict it also captured. A verdict that could not be written leaves the job
// believing that over output it never received, so a failed payload write is
// an operational error — exit 2 — even though the assertion itself did fail.
//
// The assertion result still wins whenever the verdict reached stdout;
// TestVerifyFailureIsAnAssertionError pins that side. This test is the other
// side, and it discriminates precisely because the assertion fails here too:
// a command that returned errAssertionFailed regardless of the write would
// satisfy "non-zero exit" while reporting the wrong one.
func TestVerifyReportsAFailedVerdictWriteAsOperational(t *testing.T) {
	srv := startCommandServer(t)

	cmd := newVerifyCmd()
	// No call was ever recorded, so exactly=1 cannot hold.
	ran, _, err := runCmdWithFailingStdout(t, cmd, "--addr", adminAddr(srv),
		"--method", "shop.v1.OrderService/GetOrder", "--times", "exactly=1")
	if err == nil {
		t.Fatal("verify succeeded though its verdict could not be written")
	}
	if errors.Is(err, errAssertionFailed) {
		t.Fatalf("error = %v, want an operational error: the verdict exit 1 refers to was never written", err)
	}
	if code := exitCode(ran, err); code != 2 {
		t.Fatalf("exit code = %d, want 2", code)
	}
}
