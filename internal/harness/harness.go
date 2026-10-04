// Package harness starts the pinned pi version for live contract tests under a
// hermetic environment: temporary agent and session directories, offline mode,
// and a version gate that matches the TRYGALLE_PI_* test configuration.
package harness

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/rpc"
)

// PinnedPiVersion is the only pi release the contract tests run against. It is
// the upgrade evidence gate: bumping pi requires this constant and the whole
// live suite to move together.
const (
	PinnedPiVersion = "1.0.1"
	envTests        = "TRYGALLE_PI_TESTS"
	envBin          = "TRYGALLE_PI_BIN"
)

// TRYGALLE_PI_TESTS mode values.
const (
	ModeAuto = "auto" // default (local): run with Pi 1.0.1; skip with instructions when pi is missing or another version
	ModeOn   = "on"   // CI: fail when pi is missing or the version differs from the pin
	ModeOff  = "off"  // always skip, before any binary lookup
)

// versionTimeout bounds `pi --version` so a hung binary cannot hang the suite.
const versionTimeout = 10 * time.Second

// resolveResult carries the gate decision as pure data so the unit tests can
// exercise every branch without test-control flow tricks. A non-empty
// skipReason means the live test must skip.
type resolveResult struct {
	mode       string
	bin        string
	version    string
	skipReason string
}

// resolveGate reads the test configuration and the pinned pi binary. It returns
// an error only for conditions that must fail the test: `on` with a missing
// binary or a version mismatch, a binary that exists but cannot report its
// version, or an unknown mode.
func resolveGate() (resolveResult, error) {
	mode := os.Getenv(envTests)
	if mode == "" {
		mode = ModeAuto
	}
	switch mode {
	case ModeOff:
		return resolveResult{mode: mode, skipReason: "live tests disabled (TRYGALLE_PI_TESTS=off)"}, nil
	case ModeOn, ModeAuto:
	default:
		return resolveResult{}, fmt.Errorf("invalid TRYGALLE_PI_TESTS %q (want %s, %s, or %s)", mode, ModeAuto, ModeOn, ModeOff)
	}
	bin := os.Getenv(envBin)
	if bin == "" {
		bin = "pi"
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	version := strings.TrimSpace(string(out))
	if runErr != nil {
		if !errors.Is(runErr, exec.ErrNotFound) && !errors.Is(runErr, fs.ErrNotExist) {
			return resolveResult{}, fmt.Errorf("pi binary %q failed to report its version: %v", bin, runErr)
		}
		if mode == ModeOn {
			return resolveResult{}, fmt.Errorf("pi binary %q unavailable (TRYGALLE_PI_TESTS=%s): %v", bin, mode, runErr)
		}
		return resolveResult{mode: mode, skipReason: fmt.Sprintf(
			"live tests need the pinned pi %s binary on PATH or TRYGALLE_PI_BIN", PinnedPiVersion)}, nil
	}
	if version != PinnedPiVersion {
		if mode == ModeOn {
			return resolveResult{}, fmt.Errorf("pi version %q does not match the pinned version %s", version, PinnedPiVersion)
		}
		return resolveResult{mode: mode, version: version, skipReason: fmt.Sprintf(
			"live tests need pi %s; %q reports %q (TRYGALLE_PI_TESTS=on fails instead)", PinnedPiVersion, bin, version)}, nil
	}
	return resolveResult{mode: mode, bin: bin, version: version}, nil
}

// Pi resolves the pinned pi binary per the TRYGALLE_PI_TESTS gate: it returns
// the binary path to use, skips the test with instructions in auto mode when
// the pinned version is not available, and fails the test for the errors
// resolveGate reports.
func Pi(t *testing.T) string {
	t.Helper()
	res, err := resolveGate()
	if err != nil {
		t.Fatal(err)
	}
	if res.skipReason != "" {
		t.Skip(res.skipReason)
	}
	return res.bin
}

// Start launches the given pinned pi binary in rpc mode under a hermetic
// environment and returns the connected client. It registers a bounded stop on
// test cleanup: close stdin, wait briefly, then kill.
func Start(t *testing.T, bin string, args ...string) *rpc.Client {
	t.Helper()
	if len(args) == 0 {
		args = []string{"--mode", "rpc", "--session-dir", t.TempDir()}
	}
	// These are process-wide; live tests never run in parallel.
	t.Setenv("PI_CODING_AGENT_DIR", t.TempDir())
	t.Setenv("PI_SKIP_VERSION_CHECK", "1")
	t.Setenv("PI_OFFLINE", "1")

	client, err := rpc.New(bin, args)
	if err != nil {
		t.Fatalf("start pi: %v", err)
	}
	t.Cleanup(func() {
		_ = client.CloseStdin()
		waitErr := make(chan error, 1)
		go func() { waitErr <- client.Wait() }()
		select {
		case <-waitErr:
		case <-time.After(5 * time.Second):
			_ = client.Kill()
			<-waitErr
		}
	})
	return client
}
