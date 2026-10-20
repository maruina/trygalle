package runtime

import (
	"time"
)

// Config pins the runtime configuration. Args is the full startup command line
// (for example ["--mode", "rpc", "--continue", "--session-dir", dir]);
// SessionDir records the session directory separately for diagnostics and the
// image design.
type Config struct {
	PiBin      string   // default "pi"
	Args       []string // e.g. ["--mode","rpc","--continue","--session-dir",dir]
	SessionDir string

	StartupValidationTimeout time.Duration // default 60s
	HeartbeatInterval        time.Duration // default 5m; must be positive (feeds time.NewTicker)
	ShutdownTimeout          time.Duration // default 30s; finalized by the Kubernetes design
}

// DefaultConfig returns the pinned defaults. The caller composes Args and
// SessionDir for its surface (the REPL flags, later the container entrypoint).
func DefaultConfig() Config {
	return Config{
		PiBin:                    "pi",
		StartupValidationTimeout: 60 * time.Second,
		HeartbeatInterval:        5 * time.Minute,
		ShutdownTimeout:          30 * time.Second,
	}
}

// DegradedArgs returns Args with a standalone "--continue" or "-c" element
// removed, for the degraded fresh-session restart in the startup ladder. Only
// exact standalone elements are removed; "--continue=x" stays.
func (c Config) DegradedArgs() []string {
	out := make([]string, 0, len(c.Args))
	for _, arg := range c.Args {
		if arg == "--continue" || arg == "-c" {
			continue
		}
		out = append(out, arg)
	}
	return out
}
