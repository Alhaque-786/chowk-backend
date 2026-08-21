// Package sender holds channel delivery adapters.
package sender

import (
	"context"
	"log/slog"

	"github.com/zevoxa/chowk-backend/internal/modules/notification/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// LogSender writes messages to the application log instead of sending them.
//
// This is the default in development, and it is genuinely useful rather than a
// stub: the whole notification path — template lookup, rendering, preference
// checks, dedupe, logging — runs for real, and only the final network call is
// replaced. A developer sees exactly what a customer would have received,
// including whether a placeholder failed to fill.
//
// It also means running without an SMS provider is a degraded deployment rather
// than a broken one.
type LogSender struct {
	channel domain.Channel
	log     *slog.Logger
}

func NewLogSender(channel domain.Channel, log *slog.Logger) *LogSender {
	return &LogSender{channel: channel, log: log}
}

func (s *LogSender) Channel() domain.Channel { return s.channel }

func (s *LogSender) Send(_ context.Context, msg domain.Message) (string, error) {
	// The destination is masked even here. A development log that accumulates
	// customer phone numbers is still a privacy liability, and habits formed in
	// dev are the ones that ship.
	s.log.Info("notification (not actually sent)",
		"channel", string(s.channel),
		"to", mask(msg.Destination),
		"subject", msg.Subject,
		"body", msg.Body)

	return "log_" + id.HumanCode(10), nil
}

// mask redacts the middle of a destination, keeping enough to correlate.
func mask(dest string) string {
	if len(dest) <= 6 {
		return "***"
	}
	return dest[:3] + "***" + dest[len(dest)-3:]
}

var _ domain.Sender = (*LogSender)(nil)
