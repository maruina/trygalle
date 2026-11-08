package telegram

import (
	"fmt"
	"time"
)

// defaultPollTimeout is the Telegram long-poll timeout and the base for the
// bot client's HTTP timeout.
const defaultPollTimeout = 30 * time.Second

// Config holds the transport's environment configuration. Secrets enter only
// through environment variables; diagnostics never quote them.
type Config struct {
	// Token is the Bot API token.
	Token string

	// AllowedUser is the allowlisted sender's numeric user ID; sends go to
	// its private chat.
	AllowedUser int64

	// PollTimeout is the long-poll timeout seconds.
	PollTimeout time.Duration
}

// Validate checks the configuration and stamps the poll timeout default.
// Diagnostics never quote the token.
func (c Config) Validate() (Config, error) {
	if c.Token == "" {
		return c, fmt.Errorf("telegram: missing bot token (set TRYGALLE_TELEGRAM_TOKEN)")
	}
	if c.AllowedUser == 0 {
		return c, fmt.Errorf("telegram: missing allowed user (set TRYGALLE_TELEGRAM_ALLOWED_USER)")
	}
	if c.PollTimeout <= 0 {
		c.PollTimeout = defaultPollTimeout
	}
	return c, nil
}
