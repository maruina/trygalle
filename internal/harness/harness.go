// Package harness starts the pinned pi version for live contract tests under a
// hermetic environment: temporary agent and session directories, offline mode,
// and a version gate that matches the TRYGALLE_PI_* test configuration.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
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

type piVersionCheck struct {
	mode    string
	bin     string
	version string
	err     error
}

// resolveGate reads the test configuration and the pinned pi binary. It returns
// an error only for conditions that must fail the test: `on` with a missing
// binary or a version mismatch, a binary that exists but cannot report its
// version, or an unknown mode.
func resolveGate() (resolveResult, error) {
	mode, err := configuredMode()
	if err != nil {
		return resolveResult{}, err
	}
	if mode == ModeOff {
		return resolveResult{mode: mode, skipReason: "live tests disabled (TRYGALLE_PI_TESTS=off)"}, nil
	}
	bin := os.Getenv(envBin)
	if bin == "" {
		bin = "pi"
	}
	ctx, cancel := context.WithTimeout(context.Background(), versionTimeout)
	defer cancel()
	out, runErr := exec.CommandContext(ctx, bin, "--version").CombinedOutput()
	return classifyGate(piVersionCheck{
		mode:    mode,
		bin:     bin,
		version: strings.TrimSpace(string(out)),
		err:     runErr,
	})
}

func configuredMode() (string, error) {
	mode := os.Getenv(envTests)
	if mode == "" {
		mode = ModeAuto
	}
	switch mode {
	case ModeOff, ModeOn, ModeAuto:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid TRYGALLE_PI_TESTS %q (want %s, %s, or %s)", mode, ModeAuto, ModeOn, ModeOff)
	}
}

func classifyGate(check piVersionCheck) (resolveResult, error) {
	if check.err != nil {
		if errors.Is(check.err, exec.ErrNotFound) || errors.Is(check.err, fs.ErrNotExist) {
			return unavailablePi(check)
		}
		return resolveResult{}, fmt.Errorf("pi binary %q failed to report its version: %v", check.bin, check.err)
	}
	if check.version != PinnedPiVersion {
		return versionMismatch(check)
	}
	return resolveResult{mode: check.mode, bin: check.bin, version: check.version}, nil
}

func unavailablePi(check piVersionCheck) (resolveResult, error) {
	if check.mode == ModeOn {
		return resolveResult{}, fmt.Errorf("pi binary %q unavailable (TRYGALLE_PI_TESTS=%s): %v", check.bin, check.mode, check.err)
	}
	return resolveResult{mode: check.mode, skipReason: fmt.Sprintf(
		"live tests need the pinned pi %s binary on PATH or TRYGALLE_PI_BIN", PinnedPiVersion)}, nil
}

func versionMismatch(check piVersionCheck) (resolveResult, error) {
	if check.mode == ModeOn {
		return resolveResult{}, fmt.Errorf("pi version %q does not match the pinned version %s", check.version, PinnedPiVersion)
	}
	return resolveResult{mode: check.mode, version: check.version, skipReason: fmt.Sprintf(
		"live tests need pi %s; %q reports %q (TRYGALLE_PI_TESTS=on fails instead)", PinnedPiVersion, check.bin, check.version)}, nil
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

// Option configures a harness.Start call.
type Option func(*options)

type options struct {
	args          []string
	model         *MockModel
	extensionPath string
}

// WithArgs replaces the default pi startup arguments.
func WithArgs(args ...string) Option { return func(o *options) { o.args = args } }

// WithModel adds the generated models.json with the mock provider pointing at
// m and appends --provider mock --model mock-model to the startup arguments.
func WithModel(m *MockModel) Option { return func(o *options) { o.model = m } }

// WithExtension copies one pi extension source file into the hermetic agent
// directory so Pi loads it at startup.
func WithExtension(path string) Option { return func(o *options) { o.extensionPath = path } }

// Start launches the given pinned pi binary in rpc mode under a hermetic
// environment and returns the connected client. It registers a bounded stop on
// test cleanup: close stdin, wait briefly, then kill.
func Start(t *testing.T, bin string, opts ...Option) *rpc.Client {
	t.Helper()
	o := options{}
	for _, opt := range opts {
		opt(&o)
	}
	args := o.args
	if len(args) == 0 {
		args = []string{"--mode", "rpc", "--session-dir", t.TempDir()}
	}
	// These are process-wide; live tests never run in parallel.
	agentDir := t.TempDir()
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	t.Setenv("PI_SKIP_VERSION_CHECK", "1")
	t.Setenv("PI_OFFLINE", "1")
	if o.model != nil {
		writeModelsJSON(t, agentDir, o.model.URL())
		args = append(args, "--provider", mockProvider, "--model", mockModelID)
	}
	if o.extensionPath != "" {
		installExtension(t, agentDir, o.extensionPath)
	}
	return start(t, bin, args)
}

// writeModelsJSON generates the hermetic models.json that registers the mock
// provider against the mock model server. Pi requests {baseUrl}/chat/completions,
// so the base URL carries the /v1 prefix.
func writeModelsJSON(t *testing.T, agentDir, baseURL string) {
	t.Helper()
	models := map[string]any{
		"providers": map[string]any{
			mockProvider: map[string]any{
				"baseUrl": baseURL + "/v1",
				"api":     mockAPI,
				"apiKey":  mockAPIKey,
				"models":  []any{map[string]any{"id": mockModelID}},
			},
		},
	}
	b, err := json.MarshalIndent(models, "", "  ")
	if err != nil {
		t.Fatalf("encode models.json: %v", err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), b, 0o644); err != nil {
		t.Fatalf("write models.json: %v", err)
	}
}

// installExtension copies one pi extension source file into the hermetic agent
// extension directory so Pi loads it at startup.
func installExtension(t *testing.T, agentDir, path string) {
	t.Helper()
	src, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read extension %s: %v", path, err)
	}
	extDir := filepath.Join(agentDir, "extensions")
	if err := os.MkdirAll(extDir, 0o755); err != nil {
		t.Fatalf("create extensions dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(extDir, filepath.Base(path)), src, 0o644); err != nil {
		t.Fatalf("install extension: %v", err)
	}
}

func start(t *testing.T, bin string, args []string) *rpc.Client {
	t.Helper()
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
