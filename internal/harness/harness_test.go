package harness

import (
	"os"
	"path/filepath"
	"testing"
)

// fakeBin writes an executable script that prints the given version.
func fakeBin(t *testing.T, version string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "pi")
	script := "#!/bin/sh\necho " + version + "\n"
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
	if res.mode != ModeOff || res.bin != "" || res.unavailable {
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
	if !res.unavailable {
		t.Errorf("resolve result = %+v, want unavailable", res)
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
	t.Setenv(envBin, fakeBin(t, PinnedPiVersion))
	res, err := resolveGate()
	if err != nil {
		t.Fatalf("resolveGate error: %v", err)
	}
	if res.bin == "" || res.version != PinnedPiVersion {
		t.Errorf("resolve result = %+v, want the fake 1.0.1 binary", res)
	}
}

func TestResolveGateVersionMismatchFailsInAuto(t *testing.T) {
	t.Setenv(envTests, ModeAuto)
	t.Setenv(envBin, fakeBin(t, "1.0.0"))
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure on version mismatch")
	}
}

func TestResolveGateVersionMismatchFailsInOn(t *testing.T) {
	t.Setenv(envTests, ModeOn)
	t.Setenv(envBin, fakeBin(t, "9.9.9"))
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure on version mismatch")
	}
}

func TestResolveGateInvalidModeFails(t *testing.T) {
	t.Setenv(envTests, "sometimes")
	if _, err := resolveGate(); err == nil {
		t.Fatal("resolveGate = nil error, want failure on invalid mode")
	}
}

// TestPiResolvesInstalledBinary proves the gate resolves the real pinned
// binary when it is available; it passes when pi is on PATH and fails loudly
// (via t.Fatal) when the pinned binary is not installed.
func TestPiResolvesInstalledBinary(t *testing.T) {
	if os.Getenv(envTests) == ModeOff {
		t.Skip("test gate disabled")
	}
	bin := Pi(t)
	if bin == "" {
		t.Fatal("Pi(t) returned an empty binary path")
	}
}
