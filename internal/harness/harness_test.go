package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBin writes an executable script with the given shell body.
func fakeBin(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\n" + body + "\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake bin: %v", err)
	}
	return bin
}

func TestResolveGateOffSkipsBeforeBinaryLookup(t *testing.T) {
	t.Setenv(envTests, ModeOff)
	// Point at a nonexistent binary: off must not look it up.
	t.Setenv(envBin, filepath.Join(t.TempDir(), "does-not-exist"))
	res, err := resolveGate()
	if err != nil {
		t.Fatalf("resolveGate error: %v", err)
	}
	if res.mode != ModeOff || res.bin != "" || res.skipReason == "" {
		t.Errorf("resolve result = %+v, want pure off", res)
	}
}

func TestResolveGateAutoUnavailableSkips(t *testing.T) {
	t.Setenv(envTests, ModeAuto)
	t.Setenv(envBin, filepath.Join(t.TempDir(), "does-not-exist"))
	res, err := resolveGate()
	if err != nil {
		t.Fatalf("resolveGate error: %v", err)
	}
	if res.bin != "" || res.skipReason == "" {
		t.Errorf("resolve result = %+v, want a skip", res)
	}
}

func TestResolveGateOnUnavailableFails(t *testing.T) {
	t.Setenv(envTests, ModeOn)
	t.Setenv(envBin, filepath.Join(t.TempDir(), "does-not-exist"))
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure for on with unavailable binary")
	}
}

func TestResolveGateVersionMatch(t *testing.T) {
	t.Setenv(envTests, ModeAuto)
	t.Setenv(envBin, fakeBin(t, "echo "+PinnedPiVersion))
	res, err := resolveGate()
	if err != nil {
		t.Fatalf("resolveGate error: %v", err)
	}
	if res.bin == "" || res.version != PinnedPiVersion || res.skipReason != "" {
		t.Errorf("resolve result = %+v, want the fake 1.0.1 binary", res)
	}
}

// TestResolveGateVersionMismatchSkipsInAuto proves a local machine with another
// pi version skips the live suite; only `on` (CI) enforces the pin.
func TestResolveGateVersionMismatchSkipsInAuto(t *testing.T) {
	t.Setenv(envTests, ModeAuto)
	t.Setenv(envBin, fakeBin(t, "echo 1.0.0"))
	res, err := resolveGate()
	if err != nil {
		t.Fatalf("resolveGate error: %v", err)
	}
	if res.bin != "" || res.skipReason == "" {
		t.Errorf("resolve result = %+v, want a skip on version mismatch", res)
	}
}

func TestResolveGateVersionMismatchFailsInOn(t *testing.T) {
	t.Setenv(envTests, ModeOn)
	t.Setenv(envBin, fakeBin(t, "echo 9.9.9"))
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure on version mismatch")
	}
}

// TestResolveGateBrokenBinaryFailsInAuto proves a binary that exists but
// cannot report its version fails the test instead of skipping it.
func TestResolveGateBrokenBinaryFailsInAuto(t *testing.T) {
	t.Setenv(envTests, ModeAuto)
	t.Setenv(envBin, fakeBin(t, "exit 1"))
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure for a broken binary")
	}
}

func TestResolveGateInvalidModeFails(t *testing.T) {
	t.Setenv(envTests, "sometimes")
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure on invalid mode")
	}
}
