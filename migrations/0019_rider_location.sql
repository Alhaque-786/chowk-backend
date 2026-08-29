-- 0019_rider_location
--
-- Why this change:
--
-- The delivery module's own documentation says it owns "live location" and
-- publishes rider.location_updated. Neither was true: the event name is declared
-- in internal/contracts and never published, no rider-facing route was mounted
-- anywhere, and a delivery carried no coordinates at all. The customer's tracking
-- view could name a rider and give an ETA and could not say where they were.
--
-- Two things were missing, and the first is the less obvious one.
--
-- A DELIVERY WAS NEVER LINKED TO A RIDER ACCOUNT. It carries rider_name and
-- rider_phone as free text, which is exactly right for a third-party courier --
-- we get a name and a number from their API and no user of ours is involved. It
-- gives us nothing for our OWN riders, where the rider is a person with an
-- account, a role, and a phone posting positions. rider_id is nullable because
-- both kinds of delivery are real and will stay real: the 3PL case never has one.
--
-- WHERE THE POSITIONS GO. Live position is a Redis key with a short TTL, not a
-- row per update. A rider posting every five seconds is 720 writes an hour each,
-- and a table of them would be almost entirely data nobody ever reads: the only
-- question anyone asks of a finished delivery is "roughly where did it go", and
-- once a week later, at most.
--
-- So Postgres keeps a DECIMATED track -- a point when the rider has moved far
-- enough or enough time has passed, whichever comes first. That is small enough
-- to keep and detailed enough to answer the question that gets asked: a customer
-- says the rider never came, and a line on a map settles it. Enforcing the
-- decimation in the writer rather than here, because "far enough" is a judgement
-- about GPS noise and not something a constraint can express.

ALTER TABLE deliveries
    -- ON DELETE SET NULL, not CASCADE. A rider leaving must never delete the
    -- record that a delivery happened; the order it belongs to outlives them.
    ADD COLUMN rider_id UUID REFERENCES users (id) ON DELETE SET NULL;

-- Riders ask "what am I carrying" on every app foreground, and it is always the
-- handful of deliveries that are still moving. Partial, because a finished
-- delivery is never the answer to that question and there will eventually be
-- millions of them.
CREATE INDEX deliveries_rider_active_idx
    ON deliveries (rider_id, status)
    WHERE rider_id IS NOT NULL
      AND status IN ('assigned', 'at_pickup', 'picked_up');

CREATE TABLE delivery_track_points (
    id          UUID PRIMARY KEY,
    delivery_id UUID NOT NULL REFERENCES deliveries (id) ON DELETE CASCADE,

    latitude    DOUBLE PRECISION NOT NULL,
    longitude   DOUBLE PRECISION NOT NULL,

    -- What the handset claimed, in metres. Kept because it is the only way to
    -- tell a rider who genuinely went somewhere from a phone that lost its fix
    -- and guessed. A 2km "jump" with 3km accuracy is noise, not a detour.
    accuracy_m  REAL,

    -- When the DEVICE recorded it, which is not when we received it. A rider
    -- through a basement or a dead spot uploads a burst on reconnect, and
    -- ordering those by arrival draws a track that teleports.
    recorded_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),

    CONSTRAINT delivery_track_points_lat_check CHECK (latitude BETWEEN -90 AND 90),
    CONSTRAINT delivery_track_points_lng_check CHECK (longitude BETWEEN -180 AND 180)
);

-- Every read of this table is "the track for one delivery, in order".
CREATE INDEX delivery_track_points_delivery_idx
    ON delivery_track_points (delivery_id, recorded_at);

COMMENT ON TABLE delivery_track_points IS
    'A decimated rider track, kept for disputes. Live position lives in Redis with a short TTL -- this is not written on every update.';

COMMENT ON COLUMN deliveries.rider_id IS
    'Our own rider''s user account. NULL for third-party couriers, where rider_name and rider_phone come from their API instead.';
