package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/zevoxa/chowk-backend/internal/modules/delivery/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// TrackRepository stores the decimated rider track.
//
// Decimated by the service before it gets here — see domain.Position.ShouldPersist.
// This layer writes what it is given and does not second-guess it, because "is
// this point worth keeping" is a judgement about GPS noise and belongs with the
// thresholds that define it.
type TrackRepository struct{ db *database.DB }

func NewTrackRepository(db *database.DB) *TrackRepository { return &TrackRepository{db: db} }

func (r *TrackRepository) Append(ctx context.Context, p domain.Position) error {
	const q = `
		INSERT INTO delivery_track_points
			(id, delivery_id, latitude, longitude, accuracy_m, recorded_at)
		VALUES ($1, $2, $3, $4, $5, $6)`

	if _, err := r.db.Q(ctx).Exec(ctx, q,
		id.New(), p.DeliveryID, p.Latitude, p.Longitude, p.AccuracyM, p.RecordedAt,
	); err != nil {
		return apperr.Internal(fmt.Errorf("delivery: append track point: %w", err))
	}
	return nil
}

// Latest is what decimation compares against.
//
// Ordered by recorded_at, not created_at: a rider who was out of signal uploads a
// burst on reconnect, and taking the most recently INSERTED point would compare
// the next reading against whichever one happened to arrive last.
func (r *TrackRepository) Latest(ctx context.Context, deliveryID id.ID) (*domain.Position, error) {
	const q = `
		SELECT latitude, longitude, accuracy_m, recorded_at
		FROM delivery_track_points
		WHERE delivery_id = $1
		ORDER BY recorded_at DESC
		LIMIT 1`

	p := domain.Position{DeliveryID: deliveryID}
	err := r.db.Q(ctx).QueryRow(ctx, q, deliveryID).
		Scan(&p.Latitude, &p.Longitude, &p.AccuracyM, &p.RecordedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// An empty track is the ordinary state at the start of a delivery.
			return nil, nil
		}
		return nil, apperr.Internal(fmt.Errorf("delivery: latest track point: %w", err))
	}
	return &p, nil
}

func (r *TrackRepository) List(ctx context.Context, deliveryID id.ID) ([]domain.Position, error) {
	const q = `
		SELECT latitude, longitude, accuracy_m, recorded_at
		FROM delivery_track_points
		WHERE delivery_id = $1
		ORDER BY recorded_at`

	rows, err := r.db.Q(ctx).Query(ctx, q, deliveryID)
	if err != nil {
		return nil, apperr.Internal(fmt.Errorf("delivery: list track: %w", err))
	}
	defer rows.Close()

	points := []domain.Position{}
	for rows.Next() {
		p := domain.Position{DeliveryID: deliveryID}
		if err := rows.Scan(&p.Latitude, &p.Longitude, &p.AccuracyM, &p.RecordedAt); err != nil {
			return nil, apperr.Internal(fmt.Errorf("delivery: scan track point: %w", err))
		}
		points = append(points, p)
	}
	if err := rows.Err(); err != nil {
		return nil, apperr.Internal(fmt.Errorf("delivery: iterate track: %w", err))
	}
	return points, nil
}
