package app

import (
	"context"

	"github.com/zevoxa/chowk-backend/internal/contracts"
	"github.com/zevoxa/chowk-backend/internal/modules/identity/domain"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
)

// DirectoryService implements contracts.UserDirectory: the read-only lookup other
// modules use to resolve a user and a delivery address.
//
// Separate from AuthService because the audience is different. Auth is called by
// the identity module's own handlers; this is called by OTHER modules, and
// keeping it distinct makes the published surface obvious — anything added here
// becomes visible to ordering, which is a decision worth making deliberately.
type DirectoryService struct {
	users     domain.UserRepository
	addresses domain.AddressRepository
}

func NewDirectoryService(users domain.UserRepository, addresses domain.AddressRepository) *DirectoryService {
	return &DirectoryService{users: users, addresses: addresses}
}

// GetUser returns the minimal user summary other modules need.
//
// Deliberately a projection, not the entity. Returning *domain.User would leak
// the password hash and every internal flag across a module boundary, and would
// make identity's schema part of ordering's contract.
func (s *DirectoryService) GetUser(ctx context.Context, userID id.ID) (contracts.UserSummary, error) {
	user, err := s.users.FindByID(ctx, userID)
	if err != nil {
		return contracts.UserSummary{}, err
	}
	return contracts.UserSummary{
		UserID:   user.ID,
		FullName: user.FullName,
		Phone:    user.Phone.String(),
		Email:    user.Email,
		IsActive: user.Status == domain.StatusActive,
	}, nil
}

// GetAddress returns a delivery address, scoped to its owner.
//
// The userID parameter is the authorisation check, not a convenience: without it,
// checkout could be handed any address id and would happily deliver someone
// else's order to it — and reveal that address in the response.
func (s *DirectoryService) GetAddress(ctx context.Context, userID, addressID id.ID) (contracts.Address, error) {
	addr, err := s.addresses.FindByID(ctx, userID, addressID)
	if err != nil {
		return contracts.Address{}, err
	}
	return contracts.Address{
		ID:        addr.ID,
		UserID:    addr.UserID,
		Label:     addr.Label,
		Line1:     addr.Line1,
		Line2:     addr.Line2,
		Landmark:  addr.Landmark,
		City:      addr.City,
		State:     addr.State,
		Pincode:   addr.Pincode,
		Latitude:  addr.Latitude,
		Longitude: addr.Longitude,
	}, nil
}

var _ contracts.UserDirectory = (*DirectoryService)(nil)
