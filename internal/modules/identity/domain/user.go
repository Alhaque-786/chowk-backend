// Package domain holds the identity module's entities, value objects and the
// repository interfaces it needs.
//
// This layer has no dependency on HTTP, SQL, Redis, or any framework -- it
// imports only the shared kernel. That constraint is what makes the business
// rules testable without a database and portable if this module becomes its own
// service.
//
// The repository interfaces are declared HERE, next to the code that consumes
// them, and implemented in adapter/postgres. That is dependency inversion: the
// domain declares what it needs, the adapter supplies it. The domain never
// points at the adapter.
package domain

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/zevoxa/chowk-backend/internal/platform/httpx"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/shared/paging"
	"github.com/zevoxa/chowk-backend/internal/shared/phone"
)

// UserStatus is the account lifecycle state.
type UserStatus string

const (
	// StatusPendingVerification means registered but phone not yet confirmed.
	// Such a user may browse but not place an order.
	StatusPendingVerification UserStatus = "pending_verification"
	StatusActive              UserStatus = "active"
	// StatusSuspended is a reversible block (fraud review, chargeback).
	StatusSuspended UserStatus = "suspended"
	// StatusDeleted is a soft delete. Rows are retained rather than removed
	// because orders reference them and financial records must stay intact.
	StatusDeleted UserStatus = "deleted"
)

// User is the identity aggregate root.
//
// Phone is the primary credential, not email: in India phone-first signup with
// OTP converts far better, and many customers have no email at all. Email is
// optional and used for receipts.
type User struct {
	ID           id.ID
	Phone        phone.Phone
	Email        string
	FullName     string
	PasswordHash string // empty for OTP-only accounts, which is a valid state
	Status       UserStatus
	Roles        []httpx.Role

	// KitchenID scopes staff and kitchen admins to one kitchen. Nil for
	// customers and platform admins.
	KitchenID id.ID

	PhoneVerifiedAt *time.Time
	EmailVerifiedAt *time.Time
	LastLoginAt     *time.Time

	ReferralCode string
	ReferredBy   string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// CanPlaceOrder encodes the account-side precondition for checkout in one place,
// so ordering never has to reason about identity's status values.
func (u *User) CanPlaceOrder() error {
	switch u.Status {
	case StatusActive:
	case StatusPendingVerification:
		return apperr.Forbidden("PHONE_NOT_VERIFIED", "Please verify your phone number before ordering.")
	case StatusSuspended:
		return apperr.Forbidden("ACCOUNT_SUSPENDED", "Your account is on hold. Please contact support.")
	case StatusDeleted:
		return apperr.Forbidden("ACCOUNT_DELETED", "This account no longer exists.")
	default:
		return apperr.Forbidden("ACCOUNT_INELIGIBLE", "Your account cannot place orders right now.")
	}
	if u.PhoneVerifiedAt == nil {
		return apperr.Forbidden("PHONE_NOT_VERIFIED", "Please verify your phone number before ordering.")
	}
	return nil
}

// CanAuthenticate reports whether login should be permitted at all. A suspended
// user is rejected before the password is even checked.
func (u *User) CanAuthenticate() error {
	if u.Status == StatusSuspended {
		return apperr.Forbidden("ACCOUNT_SUSPENDED", "Your account is on hold. Please contact support.")
	}
	if u.Status == StatusDeleted {
		// Deliberately indistinguishable from a wrong password, so a deleted
		// account cannot be confirmed to exist.
		return apperr.Unauthorized("INVALID_CREDENTIALS", "Incorrect phone number or password.")
	}
	return nil
}

// HasPassword distinguishes password accounts from OTP-only ones, so login can
// tell "wrong password" from "this account never had one".
func (u *User) HasPassword() bool { return u.PasswordHash != "" }

// Principal projects the user into the transport-layer identity embedded in
// access tokens.
func (u *User) Principal(sessionID id.ID) httpx.Principal {
	return httpx.Principal{
		UserID:    u.ID,
		Roles:     u.Roles,
		KitchenID: u.KitchenID,
		SessionID: sessionID,
	}
}

func (u *User) IsStaff() bool {
	for _, r := range u.Roles {
		switch r {
		case httpx.RoleKitchenStaff, httpx.RoleKitchenAdmin, httpx.RolePlatformAdmin, httpx.RoleSupport:
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Email
// ---------------------------------------------------------------------------

var emailPattern = regexp.MustCompile(`^[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}$`)

// NormalizeEmail lowercases and validates an optional email. An empty string is
// valid and means "not provided".
func NormalizeEmail(raw string) (string, error) {
	e := strings.ToLower(strings.TrimSpace(raw))
	if e == "" {
		return "", nil
	}
	if len(e) > 254 || !emailPattern.MatchString(e) {
		return "", apperr.Invalid("INVALID_EMAIL", "Enter a valid email address.").
			WithField("email", "invalid_format")
	}
	return e, nil
}

// ---------------------------------------------------------------------------
// Session -- a persisted refresh token
// ---------------------------------------------------------------------------

// Session is one logged-in device. Sessions exist so a user can log out of one
// phone without invalidating the others, and so support can kill a compromised
// session immediately -- neither is possible with stateless tokens alone.
type Session struct {
	ID               id.ID
	UserID           id.ID
	RefreshTokenHash string // SHA-256; the plaintext is never stored
	UserAgent        string
	IPAddress        string
	DeviceName       string
	ExpiresAt        time.Time
	RevokedAt        *time.Time
	// RotatedTo links to the session that replaced this one, so reuse of an old
	// token can be detected as theft rather than treated as a plain failure.
	RotatedTo  *id.ID
	CreatedAt  time.Time
	LastUsedAt time.Time
}

func (s *Session) IsValid(now time.Time) error {
	if s.RevokedAt != nil {
		return apperr.Unauthorized("SESSION_REVOKED", "This session has been signed out. Please sign in again.")
	}
	if now.After(s.ExpiresAt) {
		return apperr.Unauthorized("SESSION_EXPIRED", "Your session has expired. Please sign in again.")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Address
// ---------------------------------------------------------------------------

type AddressType string

const (
	AddressHome  AddressType = "home"
	AddressWork  AddressType = "work"
	AddressOther AddressType = "other"
)

// Address is a saved delivery destination.
//
// Latitude/Longitude are mandatory, not optional: the delivery-radius check and
// rider routing both need a point. Geocoding happens at save time so checkout
// never has to make a blocking third-party call.
type Address struct {
	ID        id.ID
	UserID    id.ID
	Type      AddressType
	Label     string
	Line1     string
	Line2     string
	Landmark  string
	City      string
	State     string
	Pincode   string
	Latitude  float64
	Longitude float64
	IsDefault bool
	// ReceiverName and ReceiverPhone let a user order to someone else without
	// changing their own profile.
	ReceiverName  string
	ReceiverPhone string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

var pincodePattern = regexp.MustCompile(`^[1-9]\d{5}$`)

// Validate checks an address is complete and deliverable.
func (a *Address) Validate() error {
	err := apperr.Invalid("INVALID_ADDRESS", "Please complete all required address fields.")
	bad := false

	if strings.TrimSpace(a.Line1) == "" {
		err.WithField("line1", "required")
		bad = true
	}
	if strings.TrimSpace(a.City) == "" {
		err.WithField("city", "required")
		bad = true
	}
	if !pincodePattern.MatchString(a.Pincode) {
		err.WithField("pincode", "must_be_6_digits")
		bad = true
	}
	// Reject the (0,0) default that an un-geocoded address would carry -- it is
	// in the Gulf of Guinea and would silently pass a naive radius check.
	if a.Latitude == 0 && a.Longitude == 0 {
		err.WithField("location", "required")
		bad = true
	}
	if a.Latitude < -90 || a.Latitude > 90 {
		err.WithField("latitude", "out_of_range")
		bad = true
	}
	if a.Longitude < -180 || a.Longitude > 180 {
		err.WithField("longitude", "out_of_range")
		bad = true
	}
	if bad {
		return err
	}
	return nil
}

// ---------------------------------------------------------------------------
// Repository ports
//
// Declared by the domain, implemented by adapter/postgres. Note every method
// takes a context: that is how the ambient transaction is propagated, which is
// what lets the app layer wrap several of these calls in one atomic commit.
// ---------------------------------------------------------------------------

type UserRepository interface {
	Create(ctx context.Context, u *User) error
	Update(ctx context.Context, u *User) error
	FindByID(ctx context.Context, userID id.ID) (*User, error)
	FindByPhone(ctx context.Context, p phone.Phone) (*User, error)
	FindByEmail(ctx context.Context, email string) (*User, error)
	FindByReferralCode(ctx context.Context, code string) (*User, error)
	// List is admin-facing; cursor-paginated because the user table is unbounded.
	List(ctx context.Context, filter UserFilter, page paging.Request) ([]*User, error)
	// ExistsByPhone is a cheap precheck that avoids hashing a password for a
	// registration that is going to fail on conflict anyway.
	ExistsByPhone(ctx context.Context, p phone.Phone) (bool, error)
}

type UserFilter struct {
	Status    UserStatus
	Role      httpx.Role
	KitchenID id.ID
	Search    string // matches name, phone or email
}

type SessionRepository interface {
	Create(ctx context.Context, s *Session) error
	FindByTokenHash(ctx context.Context, hash string) (*Session, error)
	// Revoke ends one session (logout on this device).
	Revoke(ctx context.Context, sessionID id.ID, now time.Time) error
	// RevokeAllForUser ends every session (password change, or "sign out
	// everywhere" after a suspected compromise).
	RevokeAllForUser(ctx context.Context, userID id.ID, now time.Time) error
	// MarkRotated records the replacement session, enabling refresh-token reuse
	// detection.
	MarkRotated(ctx context.Context, oldID, newID id.ID, now time.Time) error
	ListActiveForUser(ctx context.Context, userID id.ID, now time.Time) ([]*Session, error)
	// DeleteExpired is called by the cleanup worker.
	DeleteExpired(ctx context.Context, before time.Time) (int64, error)
}

type AddressRepository interface {
	Create(ctx context.Context, a *Address) error
	Update(ctx context.Context, a *Address) error
	Delete(ctx context.Context, userID, addressID id.ID) error
	FindByID(ctx context.Context, userID, addressID id.ID) (*Address, error)
	ListForUser(ctx context.Context, userID id.ID) ([]*Address, error)
	// ClearDefault is used to enforce the single-default invariant inside the
	// same transaction as setting a new one.
	ClearDefault(ctx context.Context, userID id.ID) error
}
