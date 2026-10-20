package runtime_test

import (
	"slices"
	"testing"
	"time"

	"github.com/maruina/trygalle/internal/runtime"
)

func TestDefaultConfigPinnedDefaults(t *testing.T) {
	cfg := runtime.DefaultConfig()
	if cfg.PiBin != "pi" {
		t.Errorf("PiBin = %q, want %q", cfg.PiBin, "pi")
	}
	if cfg.StartupValidationTimeout != 60*time.Second {
		t.Errorf("StartupValidationTimeout = %s, want 60s", cfg.StartupValidationTimeout)
	}
	if cfg.HeartbeatInterval != 5*time.Minute {
		t.Errorf("HeartbeatInterval = %s, want 5m", cfg.HeartbeatInterval)
	}
	if cfg.ShutdownTimeout != 30*time.Second {
		t.Errorf("ShutdownTimeout = %s, want 30s", cfg.ShutdownTimeout)
	}
}

func TestDegradedArgsDropsStandaloneContinue(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want []string
	}{
		{
			name: "removes standalone --continue",
			args: []string{"--mode", "rpc", "--continue", "--session-dir", "/data/s"},
			want: []string{"--mode", "rpc", "--session-dir", "/data/s"},
		},
		{
			name: "removes standalone -c",
			args: []string{"--mode", "rpc", "-c", "--session-dir", "/data/s"},
			want: []string{"--mode", "rpc", "--session-dir", "/data/s"},
		},
		{
			name: "keeps valued forms",
			args: []string{"--continue=x", "--cwd=y"},
			want: []string{"--continue=x", "--cwd=y"},
		},
		{
			name: "keeps non-continue args",
			args: []string{"--mode", "rpc"},
			want: []string{"--mode", "rpc"},
		},
		{
			name: "empty args",
			args: nil,
			want: []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := runtime.Config{Args: tt.args}
			got := cfg.DegradedArgs()
			if !slices.Equal(got, tt.want) {
				t.Errorf("DegradedArgs() = %q, want %q", got, tt.want)
			}
		})
	}
}
