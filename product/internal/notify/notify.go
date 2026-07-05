// Package notify sends transactional emails: the operator (Rasmus) on
// escalation and failure, the customer when a preview is ready. Like the other
// integrations it has an interface with a no-op/log fake (dev, no secrets) and
// a real implementation selected when a provider is configured.
package notify

import (
	"context"
	"log/slog"
)

// Notifier sends one email. Implementations must be safe for concurrent use and
// should never block a request path for long.
type Notifier interface {
	Send(ctx context.Context, to, subject, body string) error
}

// Noop discards every message. Used when no email provider is configured, so
// the lifecycle code can always call the notifier without nil checks.
type Noop struct{}

func (Noop) Send(context.Context, string, string, string) error { return nil }

// Log writes each message to the logger instead of sending it — useful in dev
// to see exactly what would go out.
type Log struct{ Logger *slog.Logger }

func (l Log) Send(_ context.Context, to, subject, _ string) error {
	if l.Logger != nil {
		l.Logger.Info("notify (log-only)", "to", to, "subject", subject)
	}
	return nil
}
