package app

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/platform/database"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// AddressService manages a customer's saved delivery addresses.
type AddressService struct {
	db        *database.DB
	addresses domain.AddressRepository
	clock     clock.Clock
}

func NewAddressService(db *database.DB, addresses domain.AddressRepository, clk clock.Clock) *AddressService {
	return &AddressService{db: db, addresses: addresses, clock: clk}
}

type SaveAddressInput struct {
	ID            *id.ID
	UserID        id.ID
	Type          string
	Label         string
	Line1         string
	Line2         string
	Landmark      string
	City          string
	State         string
	Pincode       string
	Latitude      float64
	Longitude     float64
	IsDefault     bool
	ReceiverName  string
	ReceiverPhone string
}

// Save creates or updates an address.
//
// Setting a new default and clearing the old one happen in ONE transaction. The
// database enforces at most one default per user via a partial unique index, so
// doing these as two separate statements would transiently violate it and fail.
func (s *AddressService) Save(ctx context.Context, in SaveAddressInput) (*domain.Address, error) {
	now := s.clock.Now()

	addrType := domain.AddressType(in.Type)
	switch addrType {
	case domain.AddressHome, domain.AddressWork, domain.AddressOther:
	default:
		addrType = domain.AddressOther
	}

	addr := &domain.Address{
		UserID:        in.UserID,
		Type:          addrType,
		Label:         in.Label,
		Line1:         in.Line1,
		Line2:         in.Line2,
		Landmark:      in.Landmark,
		City:          in.City,
		State:         in.State,
		Pincode:       in.Pincode,
		Latitude:      in.Latitude,
		Longitude:     in.Longitude,
		IsDefault:     in.IsDefault,
		ReceiverName:  in.ReceiverName,
		ReceiverPhone: in.ReceiverPhone,
		UpdatedAt:     now,
	}
	if addr.Label == "" {
		addr.Label = in.Type
	}

	if err := addr.Validate(); err != nil {
		return nil, err
	}

	err := s.db.InTx(ctx, func(ctx context.Context) error {
		// The first address a user saves becomes their default automatically:
		// otherwise checkout has nothing preselected and the customer has to make
		// a pointless choice.
		if !addr.IsDefault {
			existing, err := s.addresses.ListForUser(ctx, in.UserID)
			if err != nil {
				return err
			}
			if len(existing) == 0 {
				addr.IsDefault = true
			}
		}

		if addr.IsDefault {
			if err := s.addresses.ClearDefault(ctx, in.UserID); err != nil {
				return err
			}
		}

		if in.ID == nil {
			addr.ID = id.New()
			addr.CreatedAt = now
			return s.addresses.Create(ctx, addr)
		}
		addr.ID = *in.ID
		return s.addresses.Update(ctx, addr)
	})
	if err != nil {
		return nil, err
	}
	return addr, nil
}

func (s *AddressService) List(ctx context.Context, userID id.ID) ([]*domain.Address, error) {
	return s.addresses.ListForUser(ctx, userID)
}

func (s *AddressService) Delete(ctx context.Context, userID, addressID id.ID) error {
	return s.addresses.Delete(ctx, userID, addressID)
}
