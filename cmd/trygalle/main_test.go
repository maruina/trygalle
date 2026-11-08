package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

// commandErr runs command with one environment mapping and returns whether
// the error is the usage kind plus its rendered exit message.
func commandExit(t testing.TB, getenv func(string) string) (bool, string) {
	t.Helper()
	var stderr bytes.Buffer
	err := command(context.Background(),
		[]string{"--session-dir", t.TempDir()}, getenv, io.Discard, &stderr)
	if err == nil {
		t.Fatal("command() error = nil, want a fatal exit")
	}
	if errors.Is(err, errUsage) {
		t.Fatalf("command() error = %v, want a runtime fatal, not usage", err)
	}
	return false, err.Error()
}

func TestMissingTokenExitsFatalWithHint(t *testing.T) {
	t.Parallel()
	fatal, msg := commandExit(t, func(string) string { return "" })
	_ = fatal
	if !strings.Contains(msg, "TRYGALLE_TELEGRAM_TOKEN") {
		t.Errorf("exit message %q, want the non-secret token hint", msg)
	}
	if strings.Contains(msg, "1:") {
		t.Errorf("exit message %q must not carry secret material", msg)
	}
}

func TestMissingAllowedUserExitsFatalWithHint(t *testing.T) {
	t.Parallel()
	fatal, msg := commandExit(t, func(k string) string {
		if k == "TRYGALLE_TELEGRAM_TOKEN" {
			return "1:test-token"
		}
		return ""
	})
	_ = fatal
	if !strings.Contains(msg, "TRYGALLE_TELEGRAM_ALLOWED_USER") {
		t.Errorf("exit message %q, want the non-secret allowed-user hint", msg)
	}
}

func TestNonNumericAllowedUserExitsFatal(t *testing.T) {
	t.Parallel()
	fatal, msg := commandExit(t, func(k string) string {
		if k == "TRYGALLE_TELEGRAM_TOKEN" {
			return "1:test-token"
		}
		if k == "TRYGALLE_TELEGRAM_ALLOWED_USER" {
			return "not-a-number"
		}
		return ""
	})
	_ = fatal
	if !strings.Contains(msg, "TRYGALLE_TELEGRAM_ALLOWED_USER") {
		t.Errorf("exit message %q, want the allowed-user hint", msg)
	}
}

func TestUsageExitsTwo(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := command(context.Background(), nil, func(string) string { return "" }, io.Discard, &stderr)
	if !errors.Is(err, errUsage) {
		t.Fatalf("command() error = %v, want errUsage", err)
	}
	if got := stderr.String(); !strings.Contains(got, "--session-dir is required") {
		t.Errorf("usage output %q, want the missing flag hint and the usage text", got)
	}
}

func TestUnexpectedArgumentExitsUsage(t *testing.T) {
	t.Parallel()
	var stderr bytes.Buffer
	err := command(context.Background(),
		[]string{"--session-dir", t.TempDir(), "extra"}, func(string) string { return "" }, io.Discard, &stderr)
	if !errors.Is(err, errUsage) {
		t.Fatalf("command() error = %v, want errUsage", err)
	}
	if got := stderr.String(); !strings.Contains(got, "unexpected argument") {
		t.Errorf("usage output %q, want the unexpected-argument hint", got)
	}
}
