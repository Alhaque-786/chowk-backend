-- 0009: notifications.
--
-- Templates, device tokens, per-customer preferences, and an append-only send
-- log that doubles as the idempotency guard.
--
-- The send log is the important table. Outbox delivery is at-least-once, so
-- `order.placed` will sometimes arrive twice, and without a dedupe key the
-- customer gets the same SMS twice. That is not cosmetic: duplicate transactional
-- SMS is the single most common complaint about food-delivery apps, and every
-- send costs real money.

-- ---------------------------------------------------------------------------
-- notification_templates
--
-- Copy lives in the database, not in Go string literals, so marketing can fix a
-- typo or reword a message without a deploy. Also the only sane way to add a
-- second language later.
-- ---------------------------------------------------------------------------

CREATE TABLE notification_templates (
    id          UUID PRIMARY KEY,
    -- Key is what code refers to, e.g. "order.placed". Stable; the BODY changes.
    key         TEXT NOT NULL,
    channel     TEXT NOT NULL,
    locale      TEXT NOT NULL DEFAULT 'en',

    -- Subject is used by email and push (as the title); ignored by SMS.
    subject     TEXT NOT NULL DEFAULT '',
    -- Body uses {{param}} placeholders filled from the send request.
    body        TEXT NOT NULL,

    -- Transactional messages are exempt from marketing consent and from
    -- do-not-disturb windows; promotional ones are not. Getting this wrong is a
    -- TRAI compliance problem in India, not just an annoyance.
    is_transactional BOOLEAN NOT NULL DEFAULT true,
    -- DLT template id, required by Indian telecom regulation for SMS. An
    -- unregistered template is simply not delivered by the carrier.
    dlt_template_id  TEXT NOT NULL DEFAULT '',

    is_active   BOOLEAN NOT NULL DEFAULT true,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_templates_channel_check CHECK (
        channel IN ('push', 'sms', 'email', 'whatsapp')
    )
);

-- One active template per (key, channel, locale).
CREATE UNIQUE INDEX notification_templates_key
    ON notification_templates (key, channel, locale);

-- ---------------------------------------------------------------------------
-- device_tokens
--
-- Push targets. A user has several: phone, tablet, an old handset they still
-- occasionally open.
-- ---------------------------------------------------------------------------

CREATE TABLE device_tokens (
    id          UUID PRIMARY KEY,
    user_id     UUID NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token       TEXT NOT NULL,
    platform    TEXT NOT NULL,
    device_name TEXT NOT NULL DEFAULT '',

    -- Providers report permanently-dead tokens (app uninstalled). Recording that
    -- and stopping matters: FCM penalises senders who keep pushing to dead
    -- tokens, and it wastes quota.
    is_active     BOOLEAN NOT NULL DEFAULT true,
    failure_count INT NOT NULL DEFAULT 0,
    last_used_at  TIMESTAMPTZ,

    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT device_tokens_platform_check CHECK (platform IN ('android', 'ios', 'web'))
);

-- A token identifies a device installation globally, so it is unique on its own,
-- not per user: reinstalling after switching accounts must move the token.
CREATE UNIQUE INDEX device_tokens_token_key ON device_tokens (token);
CREATE INDEX device_tokens_user_active_idx ON device_tokens (user_id) WHERE is_active;

-- ---------------------------------------------------------------------------
-- notification_preferences
--
-- Absence means "all defaults on", so a new user needs no row. Only an explicit
-- opt-out is stored.
-- ---------------------------------------------------------------------------

CREATE TABLE notification_preferences (
    user_id           UUID PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    -- Transactional order updates cannot be disabled: a customer who ordered food
    -- is entitled to know where it is, and suppressing that generates support
    -- calls rather than goodwill.
    promotional_push  BOOLEAN NOT NULL DEFAULT true,
    promotional_sms   BOOLEAN NOT NULL DEFAULT true,
    promotional_email BOOLEAN NOT NULL DEFAULT true,
    -- Quiet hours apply to promotional messages only, in the user's local time.
    quiet_hours_start SMALLINT,
    quiet_hours_end   SMALLINT,
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT prefs_quiet_hours_check CHECK (
        (quiet_hours_start IS NULL AND quiet_hours_end IS NULL)
        OR (quiet_hours_start BETWEEN 0 AND 23 AND quiet_hours_end BETWEEN 0 AND 23)
    )
);

-- ---------------------------------------------------------------------------
-- notification_log -- append-only, and the idempotency guard
-- ---------------------------------------------------------------------------

CREATE TABLE notification_log (
    id          UUID PRIMARY KEY,
    user_id     UUID REFERENCES users (id) ON DELETE SET NULL,
    channel     TEXT NOT NULL,
    template_key TEXT NOT NULL,

    -- The resolved destination, masked before storage. Keeping full phone numbers
    -- and emails in a log nobody prunes is a privacy liability for no benefit.
    destination TEXT NOT NULL DEFAULT '',
    -- The rendered message, kept so "what exactly did we tell the customer" is
    -- answerable during a complaint.
    rendered_body TEXT NOT NULL DEFAULT '',

    -- dedupe_key is what makes at-least-once event delivery safe. Typically
    -- "<event>:<order_id>:<channel>", so a redelivered order.placed cannot send a
    -- second SMS.
    dedupe_key  TEXT NOT NULL,

    status      TEXT NOT NULL DEFAULT 'queued',
    provider    TEXT NOT NULL DEFAULT '',
    provider_ref TEXT,
    failure_reason TEXT NOT NULL DEFAULT '',
    attempts    INT NOT NULL DEFAULT 0,

    sent_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT notification_log_channel_check CHECK (
        channel IN ('push', 'sms', 'email', 'whatsapp')
    ),
    CONSTRAINT notification_log_status_check CHECK (
        status IN ('queued', 'sent', 'failed', 'suppressed')
    )
);

-- THE idempotency constraint. One send per dedupe key, enforced by the database
-- rather than by a read-then-write that races.
CREATE UNIQUE INDEX notification_log_dedupe_key ON notification_log (dedupe_key);

CREATE INDEX notification_log_user_idx ON notification_log (user_id, created_at DESC);
-- Failed sends need a human or a retry; indexed so the alert query is instant.
CREATE INDEX notification_log_failed_idx ON notification_log (created_at DESC)
    WHERE status = 'failed';

-- ---------------------------------------------------------------------------
-- Seed the transactional templates.
--
-- Deliberately plain. A customer wants to know what happened and how long, not
-- exclamation marks. Every {{param}} must be supplied by the caller or rendering
-- fails loudly rather than sending "Your order {{number}} is ready".
-- ---------------------------------------------------------------------------

INSERT INTO notification_templates (id, key, channel, subject, body, is_transactional) VALUES
    (gen_random_uuid(), 'order.placed', 'sms', '',
     'Order {{number}} confirmed at {{kitchen}}. Total Rs{{total}}. We will let you know when it is on the way.', true),
    (gen_random_uuid(), 'order.placed', 'push', 'Order confirmed',
     '{{kitchen}} has your order {{number}}. Arriving in about {{eta}} minutes.', true),

    (gen_random_uuid(), 'order.confirmed', 'push', 'Order accepted',
     '{{kitchen}} accepted your order and will start cooking shortly.', true),

    (gen_random_uuid(), 'order.preparing', 'push', 'Being prepared',
     'Your food is being cooked now.', true),

    (gen_random_uuid(), 'order.ready', 'push', 'Ready for pickup',
     'Your order is packed and waiting for a rider.', true),

    (gen_random_uuid(), 'order.picked_up', 'sms', '',
     'Order {{number}} is on the way. {{rider}} is delivering it. Call {{rider_phone}} if needed.', true),
    (gen_random_uuid(), 'order.picked_up', 'push', 'On the way',
     '{{rider}} is bringing your order. About {{eta}} minutes.', true),

    (gen_random_uuid(), 'order.delivered', 'push', 'Delivered',
     'Your order has arrived. Enjoy the food.', true),

    (gen_random_uuid(), 'order.cancelled', 'sms', '',
     'Order {{number}} was cancelled. {{reason}} Any payment will be refunded within 5-7 working days.', true),
    (gen_random_uuid(), 'order.cancelled', 'push', 'Order cancelled',
     'Order {{number}} was cancelled. {{reason}}', true),

    (gen_random_uuid(), 'payment.failed', 'sms', '',
     'Payment for order {{number}} did not go through. Nothing has been charged. Please try again.', true),

    -- Staff-facing.
    (gen_random_uuid(), 'kitchen.new_order', 'push', 'New order',
     'Order {{number}}: {{items}}. Rs{{total}}.', true),
    (gen_random_uuid(), 'kitchen.stock_low', 'push', 'Stock running low',
     '{{ingredient}} is down to {{on_hand}}{{unit}}. Reorder level is {{reorder_level}}{{unit}}.', true);
