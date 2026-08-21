// Package domain holds the notification module's entities and ports.
//
// Notification is the module that finally justifies the event bus. It subscribes
// to nearly everything and is subscribed to by nothing: ordering must not know
// that placing an order sends an SMS.
//
// Two rules run through all of it:
//
//  1. Every send is IDEMPOTENT on a dedupe key. Outbox delivery is at-least-once,
//     so a redelivered order.placed must not send a second SMS. Duplicate
//     transactional SMS is both a cost and the most common complaint about food
//     apps.
//  2. A provider outage DEGRADES, it does not fail. A dead SMS gateway must not
//     stop the push, and neither must stop the order.
package domain

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// Channel is a delivery mechanism.
type Channel string

const (
	ChannelPush     Channel = "push"
	ChannelSMS      Channel = "sms"
	ChannelEmail    Channel = "email"
	ChannelWhatsApp Channel = "whatsapp"
)

func (c Channel) Valid() bool {
	switch c {
	case ChannelPush, ChannelSMS, ChannelEmail, ChannelWhatsApp:
		return true
	}
	return false
}

// Template is a message with placeholders.
type Template struct {
	ID      id.ID
	Key     string
	Channel Channel
	Locale  string
	Subject string
	Body    string

	// IsTransactional exempts the message from marketing consent and quiet hours.
	// An order update is transactional; a discount offer is not, and treating the
	// second as the first is a TRAI compliance problem in India.
	IsTransactional bool
	DLTTemplateID   string

	IsActive bool
}

// placeholder matches {{param}} with optional surrounding whitespace.
var placeholder = regexp.MustCompile(`\{\{\s*(\w+)\s*\}\}`)

// Render fills the template from params.
//
// Returns an error on a MISSING param rather than substituting empty. Sending
// "Your order  is ready" or leaving a literal "{{number}}" in a customer's SMS is
// worse than failing and retrying: the first looks broken, the second looks
// unprofessional, and both are unrecoverable once sent.
func (t *Template) Render(params map[string]string) (subject, body string, err error) {
	var missing []string

	fill := func(text string) string {
		return placeholder.ReplaceAllStringFunc(text, func(match string) string {
			name := placeholder.FindStringSubmatch(match)[1]
			value, ok := params[name]
			if !ok || value == "" {
				missing = append(missing, name)
				return match
			}
			return value
		})
	}

	subject = fill(t.Subject)
	body = fill(t.Body)

	if len(missing) > 0 {
		return "", "", apperr.Internal(
			// Internal, not Invalid: a missing param is a programming error in the
			// publisher, not something a user did.
			&templateError{key: t.Key, missing: dedupe(missing)})
	}
	return subject, body, nil
}

type templateError struct {
	key     string
	missing []string
}

func (e *templateError) Error() string {
	return "notification: template " + e.key + " is missing params: " + strings.Join(e.missing, ", ")
}

func dedupe(in []string) []string {
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, v := range in {
		if _, dup := seen[v]; dup {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	return out
}

// DeviceToken is one push target.
type DeviceToken struct {
	ID           id.ID
	UserID       id.ID
	Token        string
	Platform     string
	DeviceName   string
	IsActive     bool
	FailureCount int
	LastUsedAt   *time.Time
}

// maxTokenFailures is when a token is considered dead.
//
// Providers penalise senders who keep pushing to uninstalled apps, so retiring a
// token after a few hard failures protects deliverability for everyone else.
const maxTokenFailures = 5

func (d *DeviceToken) ShouldRetire() bool { return d.FailureCount >= maxTokenFailures }

// Preferences is a user's opt-outs. The zero value means "everything on".
type Preferences struct {
	UserID           id.ID
	PromotionalPush  bool
	PromotionalSMS   bool
	PromotionalEmail bool
	QuietHoursStart  *int16
	QuietHoursEnd    *int16
}

// DefaultPreferences is what a user with no stored row gets.
func DefaultPreferences(userID id.ID) Preferences {
	return Preferences{
		UserID:           userID,
		PromotionalPush:  true,
		PromotionalSMS:   true,
		PromotionalEmail: true,
	}
}

// Allows reports whether a message may be sent on a channel.
//
// TRANSACTIONAL MESSAGES ALWAYS PASS. A customer who ordered food is entitled to
// know where it is, regardless of marketing preferences or the hour — suppressing
// that generates support calls, not goodwill. Only promotional messages are
// filtered.
func (p Preferences) Allows(channel Channel, isTransactional bool, localHour int) bool {
	if isTransactional {
		return true
	}

	switch channel {
	case ChannelPush:
		if !p.PromotionalPush {
			return false
		}
	case ChannelSMS:
		if !p.PromotionalSMS {
			return false
		}
	case ChannelEmail:
		if !p.PromotionalEmail {
			return false
		}
	}

	return !p.inQuietHours(localHour)
}

// inQuietHours handles windows that wrap midnight (22:00 to 08:00).
func (p Preferences) inQuietHours(hour int) bool {
	if p.QuietHoursStart == nil || p.QuietHoursEnd == nil {
		return false
	}
	start, end := int(*p.QuietHoursStart), int(*p.QuietHoursEnd)

	if start == end {
		return false
	}
	if start < end {
		return hour >= start && hour < end
	}
	// Wraps midnight.
	return hour >= start || hour < end
}

// Status is the outcome of a send.
type Status string

const (
	StatusQueued Status = "queued"
	StatusSent   Status = "sent"
	StatusFailed Status = "failed"
	// StatusSuppressed means we deliberately did not send: opted out, quiet
	// hours, or no destination. Recorded rather than silently skipped, so
	// "why didn't the customer get told" is answerable.
	StatusSuppressed Status = "suppressed"
)

// LogEntry is one send attempt.
type LogEntry struct {
	ID            id.ID
	UserID        *id.ID
	Channel       Channel
	TemplateKey   string
	Destination   string
	RenderedBody  string
	DedupeKey     string
	Status        Status
	Provider      string
	ProviderRef   string
	FailureReason string
	Attempts      int
	SentAt        *time.Time
	CreatedAt     time.Time
}

// ---------------------------------------------------------------------------
// Sender port -- one per channel
// ---------------------------------------------------------------------------

// Sender delivers a rendered message on one channel.
//
// One interface per channel rather than a single Send(channel, ...) so each
// provider can be swapped, guarded and rate-limited independently. An SMS gateway
// and a push service have completely different failure modes and cost profiles,
// and sharing a circuit breaker between them would let one take out the other.
type Sender interface {
	Channel() Channel
	// Send delivers the message, returning the provider's reference for
	// reconciliation.
	Send(ctx context.Context, msg Message) (providerRef string, err error)
}

// Message is a rendered, addressed notification.
type Message struct {
	// Destination is a phone number, email, or device token depending on channel.
	Destination string
	Subject     string
	Body        string
	// DLTTemplateID is required for Indian SMS: an unregistered template is
	// silently dropped by the carrier.
	DLTTemplateID string
	// Data carries structured payload for push, so the app can deep-link to the
	// order rather than just showing text.
	Data map[string]string
}

// ---------------------------------------------------------------------------
// Ports
// ---------------------------------------------------------------------------

type TemplateRepository interface {
	Find(ctx context.Context, key string, channel Channel, locale string) (*Template, error)
	// FindForKey returns every active channel for a key, so one event can fan out
	// to push AND sms without the caller enumerating channels.
	FindForKey(ctx context.Context, key, locale string) ([]*Template, error)
	Upsert(ctx context.Context, t *Template) error
}

type LogRepository interface {
	// Claim atomically reserves a dedupe key, reporting false if this
	// notification was already sent. That boolean IS the idempotency guarantee.
	Claim(ctx context.Context, entry *LogEntry) (claimed bool, err error)
	MarkSent(ctx context.Context, entryID id.ID, provider, providerRef string, at time.Time) error
	MarkFailed(ctx context.Context, entryID id.ID, reason string) error
	MarkSuppressed(ctx context.Context, entryID id.ID, reason string) error
	ListForUser(ctx context.Context, userID id.ID, limit int) ([]*LogEntry, error)
}

type DeviceRepository interface {
	Register(ctx context.Context, token *DeviceToken) error
	ListActiveForUser(ctx context.Context, userID id.ID) ([]*DeviceToken, error)
	RecordFailure(ctx context.Context, token string) error
	Deactivate(ctx context.Context, token string) error
}

type PreferenceRepository interface {
	Get(ctx context.Context, userID id.ID) (Preferences, error)
	Save(ctx context.Context, prefs Preferences) error
}
