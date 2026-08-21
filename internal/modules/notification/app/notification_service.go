// Package app holds the notification module's use cases.
package app

import (
	"context"
	"time"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/notification/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/logger"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// NotificationService renders and delivers messages.
type NotificationService struct {
	templates domain.TemplateRepository
	logs      domain.LogRepository
	devices   domain.DeviceRepository
	prefs     domain.PreferenceRepository
	users     contracts.UserDirectory

	// senders keyed by channel. A missing channel means "not configured", which
	// suppresses rather than errors: running without an SMS provider should be a
	// degraded deployment, not a broken one.
	senders map[domain.Channel]domain.Sender

	clock clock.Clock
}

func NewNotificationService(
	templates domain.TemplateRepository,
	logs domain.LogRepository,
	devices domain.DeviceRepository,
	prefs domain.PreferenceRepository,
	users contracts.UserDirectory,
	senders []domain.Sender,
	clk clock.Clock,
) *NotificationService {
	byChannel := make(map[domain.Channel]domain.Sender, len(senders))
	for _, s := range senders {
		byChannel[s.Channel()] = s
	}
	return &NotificationService{
		templates: templates, logs: logs, devices: devices, prefs: prefs,
		users: users, senders: byChannel, clock: clk,
	}
}

// Send implements contracts.Notifier.
//
// Fans out across the requested channels INDEPENDENTLY. One channel failing must
// not stop the others: a dead SMS gateway should still let the push through, and
// neither should fail the caller — the order already happened, and telling the
// customer is best-effort by nature.
//
// Returns nil unless something structural is wrong (no template at all). Per
// channel outcomes are recorded in the log, which is where "why didn't the
// customer get told" gets answered.
func (s *NotificationService) Send(ctx context.Context, req contracts.NotificationRequest) error {
	log := logger.From(ctx)

	if req.DedupeKey == "" {
		// Without a dedupe key, at-least-once event delivery means duplicate SMS.
		// Refusing is better than silently sending twice.
		return apperr.Internal(errMissingDedupeKey)
	}

	channels := req.Channels
	if len(channels) == 0 {
		return nil
	}

	// Resolve the recipient once, not per channel.
	user, err := s.users.GetUser(ctx, req.UserID)
	if err != nil {
		return err
	}
	prefs, err := s.prefs.Get(ctx, req.UserID)
	if err != nil {
		return err
	}

	localHour := s.clock.Now().In(clock.IST).Hour()

	for _, requested := range channels {
		channel := domain.Channel(requested)
		if !channel.Valid() {
			continue
		}

		// Each channel gets its own dedupe key: a redelivered event must not send
		// two SMS, but push AND sms for the same event are both legitimate.
		dedupeKey := req.DedupeKey + ":" + string(channel)

		if err := s.sendOne(ctx, channel, req, user, prefs, dedupeKey, localHour); err != nil {
			// Logged, not returned. The caller is an event handler; failing it would
			// make the relay retry the whole fan-out, re-sending the channels that
			// already succeeded.
			log.Error("notification failed",
				"channel", channel, "template", req.TemplateID,
				"dedupe_key", dedupeKey, "error", err)
		}
	}
	return nil
}

func (s *NotificationService) sendOne(
	ctx context.Context,
	channel domain.Channel,
	req contracts.NotificationRequest,
	user contracts.UserSummary,
	prefs domain.Preferences,
	dedupeKey string,
	localHour int,
) error {
	template, err := s.templates.Find(ctx, req.TemplateID, channel, "en")
	if err != nil {
		// No template for this channel is normal: order.preparing has a push
		// template but deliberately no SMS, because an SMS per status change is
		// spam.
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil
		}
		return err
	}

	subject, body, err := template.Render(req.Params)
	if err != nil {
		// A missing param is a publisher bug. Fail loudly rather than sending a
		// message with a literal {{placeholder}} in it.
		return err
	}

	entry := &domain.LogEntry{
		ID:           id.New(),
		UserID:       &req.UserID,
		Channel:      channel,
		TemplateKey:  req.TemplateID,
		RenderedBody: body,
		DedupeKey:    dedupeKey,
		Status:       domain.StatusQueued,
		CreatedAt:    s.clock.Now(),
	}

	// THE idempotency gate. A unique index on dedupe_key arbitrates, so two
	// concurrent deliveries of the same event cannot both pass.
	claimed, err := s.logs.Claim(ctx, entry)
	if err != nil {
		return err
	}
	if !claimed {
		logger.From(ctx).Debug("notification already sent, skipping",
			"dedupe_key", dedupeKey)
		return nil
	}

	// Preference check AFTER claiming, so a suppressed message still occupies its
	// dedupe key and is not retried on every redelivery.
	if !prefs.Allows(channel, template.IsTransactional, localHour) {
		return s.logs.MarkSuppressed(ctx, entry.ID, "user preference or quiet hours")
	}

	sender, ok := s.senders[channel]
	if !ok {
		return s.logs.MarkSuppressed(ctx, entry.ID, "no provider configured for this channel")
	}

	destinations, err := s.resolveDestinations(ctx, channel, user)
	if err != nil {
		return err
	}
	if len(destinations) == 0 {
		// No phone, no email, no registered device. Not an error — plenty of users
		// have no email, and a customer who never opened the app has no token.
		return s.logs.MarkSuppressed(ctx, entry.ID, "no destination for this channel")
	}

	msg := domain.Message{
		Subject:       subject,
		Body:          body,
		DLTTemplateID: template.DLTTemplateID,
		Data:          req.Params,
	}

	// Push fans out to every device the user has; SMS and email have one
	// destination. Partial success counts as sent: the customer got the message on
	// at least one device, which is what matters.
	var (
		lastErr     error
		providerRef string
		delivered   bool
	)
	for _, destination := range destinations {
		msg.Destination = destination

		ref, err := sender.Send(ctx, msg)
		if err != nil {
			lastErr = err
			// Track dead push tokens so they get retired rather than pushed to
			// forever, which damages sender reputation with the provider.
			if channel == domain.ChannelPush {
				_ = s.devices.RecordFailure(ctx, destination)
			}
			continue
		}
		delivered = true
		providerRef = ref
	}

	if !delivered {
		reason := "delivery failed"
		if lastErr != nil {
			reason = lastErr.Error()
		}
		if err := s.logs.MarkFailed(ctx, entry.ID, reason); err != nil {
			return err
		}
		return lastErr
	}

	return s.logs.MarkSent(ctx, entry.ID, string(channel), providerRef, s.clock.Now())
}

// resolveDestinations turns a user into channel-specific addresses.
func (s *NotificationService) resolveDestinations(ctx context.Context, channel domain.Channel, user contracts.UserSummary) ([]string, error) {
	switch channel {
	case domain.ChannelSMS, domain.ChannelWhatsApp:
		if user.Phone == "" {
			return nil, nil
		}
		return []string{user.Phone}, nil

	case domain.ChannelEmail:
		if user.Email == "" {
			return nil, nil
		}
		return []string{user.Email}, nil

	case domain.ChannelPush:
		tokens, err := s.devices.ListActiveForUser(ctx, user.UserID)
		if err != nil {
			return nil, err
		}
		out := make([]string, 0, len(tokens))
		for _, t := range tokens {
			out = append(out, t.Token)
		}
		return out, nil
	}
	return nil, nil
}

// RegisterDevice records a push token.
func (s *NotificationService) RegisterDevice(ctx context.Context, userID id.ID, token, platform, deviceName string) error {
	if token == "" {
		return apperr.Invalid("MISSING_TOKEN", "A device token is required.")
	}
	switch platform {
	case "android", "ios", "web":
	default:
		return apperr.Invalid("INVALID_PLATFORM", "Platform must be android, ios or web.")
	}

	now := s.clock.Now()
	return s.devices.Register(ctx, &domain.DeviceToken{
		ID:         id.New(),
		UserID:     userID,
		Token:      token,
		Platform:   platform,
		DeviceName: deviceName,
		IsActive:   true,
		LastUsedAt: &now,
	})
}

func (s *NotificationService) UnregisterDevice(ctx context.Context, token string) error {
	return s.devices.Deactivate(ctx, token)
}

func (s *NotificationService) GetPreferences(ctx context.Context, userID id.ID) (domain.Preferences, error) {
	return s.prefs.Get(ctx, userID)
}

func (s *NotificationService) SavePreferences(ctx context.Context, prefs domain.Preferences) error {
	if prefs.QuietHoursStart != nil && (*prefs.QuietHoursStart < 0 || *prefs.QuietHoursStart > 23) {
		return apperr.Invalid("INVALID_QUIET_HOURS", "Quiet hours must be between 0 and 23.")
	}
	if prefs.QuietHoursEnd != nil && (*prefs.QuietHoursEnd < 0 || *prefs.QuietHoursEnd > 23) {
		return apperr.Invalid("INVALID_QUIET_HOURS", "Quiet hours must be between 0 and 23.")
	}
	return s.prefs.Save(ctx, prefs)
}

func (s *NotificationService) History(ctx context.Context, userID id.ID, limit int) ([]*domain.LogEntry, error) {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	return s.logs.ListForUser(ctx, userID, limit)
}

// ChannelsAvailable reports which channels have a configured provider, for the
// admin health view. "Why did nobody get an SMS" is usually "no SMS provider is
// configured in this environment".
func (s *NotificationService) ChannelsAvailable() []string {
	out := make([]string, 0, len(s.senders))
	for channel := range s.senders {
		out = append(out, string(channel))
	}
	return out
}

var errMissingDedupeKey = &missingDedupeKeyError{}

type missingDedupeKeyError struct{}

func (*missingDedupeKeyError) Error() string {
	return "notification: a dedupe key is required; without one, at-least-once event delivery sends duplicates"
}

var _ contracts.Notifier = (*NotificationService)(nil)

// unused guards against an import being dropped by a refactor.
var _ = time.Time{}
