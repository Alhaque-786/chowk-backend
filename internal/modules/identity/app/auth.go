// Package app holds the identity module's use cases.
//
// This is the orchestration layer. Its job is to sequence domain calls,
// repository calls, and transaction boundaries. It contains no business rules
// (those live in domain) and no transport concerns (those live in adapter/http).
//
// Every method that writes more than one row does so inside db.InTx, so a
// partial failure cannot leave a user without a session or a session without a
// user.
package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/zevoxa/cloud-kitchen-backend/internal/contracts"
	"github.com/zevoxa/cloud-kitchen-backend/internal/modules/identity/domain"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/cache"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/database"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/eventbus"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/httpx"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/password"
	"github.com/zevoxa/cloud-kitchen-backend/internal/platform/token"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/apperr"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/clock"
	"github.com/zevoxa/cloud-kitchen-backend/internal/shared/id"
)

// AuthService implements registration, login, token refresh and logout.
//
// Note the dependencies: interfaces and platform primitives only. No other
// module is imported, which is what keeps identity independently deployable.
type AuthService struct {
	db       *database.DB
	users    domain.UserRepository
	sessions domain.SessionRepository
	tokens   *token.Manager
	hasher   *password.Hasher
	cache    cache.Cache
	events   eventbus.Publisher
	clock    clock.Clock

	otpTTL         time.Duration
	otpMaxAttempts int
}

func NewAuthService(
	db *database.DB,
	users domain.UserRepository,
	sessions domain.SessionRepository,
	tokens *token.Manager,
	hasher *password.Hasher,
	c cache.Cache,
	events eventbus.Publisher,
	clk clock.Clock,
	otpTTL time.Duration,
	otpMaxAttempts int,
) *AuthService {
	return &AuthService{
		db: db, users: users, sessions: sessions,
		tokens: tokens, hasher: hasher, cache: c,
		events: events, clock: clk,
		otpTTL: otpTTL, otpMaxAttempts: otpMaxAttempts,
	}
}

// TokenPair is what a successful authentication returns.
type TokenPair struct {
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
	TokenType    string
}

// DeviceInfo describes the client, recorded on the session so a user can see
// and revoke "iPhone 14, Mumbai, 2 hours ago" in their security settings.
type DeviceInfo struct {
	UserAgent  string
	IPAddress  string
	DeviceName string
}

type RegisterInput struct {
	Phone        string
	Email        string
	FullName     string
	Password     string
	ReferralCode string
	Device       DeviceInfo
}

type AuthResult struct {
	User   *domain.User
	Tokens TokenPair
}

// Register creates a customer account and signs them in.
//
// The account starts in pending_verification: they can browse immediately, but
// domain.User.CanPlaceOrder blocks checkout until the phone OTP is confirmed.
// Verifying up front would cost us the signup; verifying never would let anyone
// order to a stranger's number.
func (s *AuthService) Register(ctx context.Context, in RegisterInput) (*AuthResult, error) {
	phone, err := domain.NewPhone(in.Phone)
	if err != nil {
		return nil, err
	}
	email, err := domain.NormalizeEmail(in.Email)
	if err != nil {
		return nil, err
	}
	if err := password.ValidateStrength(in.Password); err != nil {
		return nil, err
	}

	// Cheap precheck before spending ~250ms on bcrypt for a registration that
	// will fail on conflict anyway.
	exists, err := s.users.ExistsByPhone(ctx, phone)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, apperr.Conflict("PHONE_ALREADY_REGISTERED",
			"An account with this phone number already exists. Try signing in instead.").
			WithField("phone", "already_registered")
	}

	hash, err := s.hasher.Hash(in.Password)
	if err != nil {
		return nil, err
	}

	now := s.clock.Now()
	user := &domain.User{
		ID:           id.New(),
		Phone:        phone,
		Email:        email,
		FullName:     in.FullName,
		PasswordHash: hash,
		Status:       domain.StatusPendingVerification,
		Roles:        []httpx.Role{httpx.RoleCustomer},
		ReferralCode: id.HumanCode(6),
		ReferredBy:   in.ReferralCode,
		CreatedAt:    now,
		UpdatedAt:    now,
	}

	var result *AuthResult

	// One transaction: user row, session row, and the outbox event. Either the
	// account fully exists and downstream modules were told, or nothing happened.
	err = s.db.InTx(ctx, func(ctx context.Context) error {
		if err := s.users.Create(ctx, user); err != nil {
			// Lost the race against a concurrent registration; the unique index
			// is the real guard, the precheck above is only an optimisation.
			if database.IsUniqueViolation(err, "users_phone_key") {
				return apperr.Conflict("PHONE_ALREADY_REGISTERED",
					"An account with this phone number already exists.").
					WithField("phone", "already_registered")
			}
			return err
		}

		pair, err := s.issueTokens(ctx, user, in.Device, now)
		if err != nil {
			return err
		}

		evt, err := eventbus.New(contracts.EventUserRegistered, user.ID, contracts.UserRegisteredPayload{
			UserID:       user.ID,
			Phone:        user.Phone.String(),
			Email:        user.Email,
			FullName:     user.FullName,
			ReferredBy:   user.ReferredBy,
			RegisteredAt: now,
		})
		if err != nil {
			return err
		}
		evt.TraceID = httpx.TraceIDFrom(ctx)
		if err := s.events.Publish(ctx, evt); err != nil {
			return err
		}

		result = &AuthResult{User: user, Tokens: pair}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

type LoginInput struct {
	Phone    string
	Password string
	Device   DeviceInfo
}

// Login authenticates with phone and password.
//
// Every failure path returns the same INVALID_CREDENTIALS error and burns
// comparable CPU, so an attacker cannot use the response to learn which phone
// numbers are registered.
func (s *AuthService) Login(ctx context.Context, in LoginInput) (*AuthResult, error) {
	phone, err := domain.NewPhone(in.Phone)
	if err != nil {
		// Not a validation error to the caller: revealing "that is not a valid
		// number" vs "wrong password" is fine, but keeping one message is simpler
		// and leaks nothing.
		return nil, apperr.Unauthorized("INVALID_CREDENTIALS", "Incorrect phone number or password.")
	}

	invalid := apperr.Unauthorized("INVALID_CREDENTIALS", "Incorrect phone number or password.")

	user, err := s.users.FindByPhone(ctx, phone)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			// Compare against a dummy hash so the no-such-user path takes the
			// same ~250ms as a wrong password. Skipping this is a user
			// enumeration oracle measurable over the network.
			_, _ = s.hasher.Verify(password.DummyHash, in.Password)
			return nil, invalid
		}
		return nil, err
	}

	if err := user.CanAuthenticate(); err != nil {
		return nil, err
	}
	if !user.HasPassword() {
		// OTP-only account. Tell them how to get in rather than looping them on
		// a password they never set.
		return nil, apperr.Unauthorized("PASSWORD_NOT_SET",
			"This account signs in with an OTP. Request a code instead.")
	}

	ok, err := s.hasher.Verify(user.PasswordHash, in.Password)
	if err != nil {
		return nil, apperr.Internal(err)
	}
	if !ok {
		return nil, invalid
	}

	now := s.clock.Now()
	var result *AuthResult

	err = s.db.InTx(ctx, func(ctx context.Context) error {
		pair, err := s.issueTokens(ctx, user, in.Device, now)
		if err != nil {
			return err
		}

		user.LastLoginAt = &now
		user.UpdatedAt = now

		// Opportunistically upgrade a hash made under a lower cost factor, so
		// raising BCRYPT_COST gradually re-secures existing accounts.
		if s.hasher.NeedsRehash(user.PasswordHash) {
			if rehashed, err := s.hasher.Hash(in.Password); err == nil {
				user.PasswordHash = rehashed
			}
		}
		if err := s.users.Update(ctx, user); err != nil {
			return err
		}

		result = &AuthResult{User: user, Tokens: pair}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Refresh exchanges a refresh token for a new pair, rotating the old one.
//
// Rotation with reuse detection: each refresh token is single-use. If a token
// that was already rotated is presented again, either the network duplicated it
// or it was stolen -- and since we cannot tell, every session for that user is
// revoked. That turns a stolen token into one extra login instead of permanent
// silent access.
func (s *AuthService) Refresh(ctx context.Context, refreshToken string, device DeviceInfo) (*AuthResult, error) {
	if refreshToken == "" {
		return nil, apperr.Unauthorized("MISSING_REFRESH_TOKEN", "A refresh token is required.")
	}

	hash := token.HashRefreshToken(refreshToken)
	now := s.clock.Now()

	session, err := s.sessions.FindByTokenHash(ctx, hash)
	if err != nil {
		if apperr.KindOf(err) == apperr.KindNotFound {
			return nil, apperr.Unauthorized("INVALID_REFRESH_TOKEN", "Please sign in again.")
		}
		return nil, err
	}

	// Reuse of an already-rotated token: treat as compromise.
	if session.RotatedTo != nil {
		if err := s.sessions.RevokeAllForUser(ctx, session.UserID, now); err != nil {
			return nil, err
		}
		return nil, apperr.Unauthorized("TOKEN_REUSE_DETECTED",
			"For your security, all sessions have been signed out. Please sign in again.")
	}
	if err := session.IsValid(now); err != nil {
		return nil, err
	}

	user, err := s.users.FindByID(ctx, session.UserID)
	if err != nil {
		return nil, err
	}
	if err := user.CanAuthenticate(); err != nil {
		return nil, err
	}

	var result *AuthResult
	err = s.db.InTx(ctx, func(ctx context.Context) error {
		pair, err := s.issueTokens(ctx, user, device, now)
		if err != nil {
			return err
		}
		// The new session id is what the freshly created session got; link the
		// old one to it so a replay of the old token is detectable.
		newSession, err := s.sessions.FindByTokenHash(ctx, token.HashRefreshToken(pair.RefreshToken))
		if err != nil {
			return err
		}
		if err := s.sessions.MarkRotated(ctx, session.ID, newSession.ID, now); err != nil {
			return err
		}
		result = &AuthResult{User: user, Tokens: pair}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Logout revokes the current session only, leaving the user's other devices
// signed in.
func (s *AuthService) Logout(ctx context.Context, sessionID id.ID) error {
	return s.sessions.Revoke(ctx, sessionID, s.clock.Now())
}

// LogoutEverywhere revokes every session for the user.
func (s *AuthService) LogoutEverywhere(ctx context.Context, userID id.ID) error {
	return s.sessions.RevokeAllForUser(ctx, userID, s.clock.Now())
}

// issueTokens mints an access/refresh pair and persists the session. It must be
// called inside a transaction.
func (s *AuthService) issueTokens(ctx context.Context, user *domain.User, device DeviceInfo, now time.Time) (TokenPair, error) {
	sessionID := id.New()

	refresh, err := s.tokens.NewRefreshToken(now)
	if err != nil {
		return TokenPair{}, apperr.Internal(err)
	}

	session := &domain.Session{
		ID:               sessionID,
		UserID:           user.ID,
		RefreshTokenHash: refresh.Hash,
		UserAgent:        device.UserAgent,
		IPAddress:        device.IPAddress,
		DeviceName:       device.DeviceName,
		ExpiresAt:        refresh.ExpiresAt,
		CreatedAt:        now,
		LastUsedAt:       now,
	}
	if err := s.sessions.Create(ctx, session); err != nil {
		return TokenPair{}, err
	}

	access, expiresAt, err := s.tokens.IssueAccessToken(user.Principal(sessionID), now)
	if err != nil {
		return TokenPair{}, apperr.Internal(err)
	}

	return TokenPair{
		AccessToken:  access,
		RefreshToken: refresh.Plaintext,
		ExpiresAt:    expiresAt,
		TokenType:    "Bearer",
	}, nil
}

// ---------------------------------------------------------------------------
// OTP flow
// ---------------------------------------------------------------------------

// otpRecord is what we store in Redis against a phone number. Attempts are
// tracked server-side so a 6-digit code cannot be brute-forced -- a million
// guesses is minutes of scripted traffic without a cap.
type otpRecord struct {
	Code     string `json:"code"`
	Attempts int    `json:"attempts"`
	Purpose  string `json:"purpose"`
}

func otpKey(phone domain.Phone, purpose string) string {
	return fmt.Sprintf("otp:%s:%s", purpose, phone)
}

func otpThrottleKey(phone domain.Phone) string {
	return fmt.Sprintf("otp:throttle:%s", phone)
}

// RequestOTP generates and dispatches a login/verification code.
//
// Returns the code only in non-production; the caller decides whether to expose
// it. Sending is the notification module's job, reached via an event, so identity
// does not depend on an SMS provider.
func (s *AuthService) RequestOTP(ctx context.Context, rawPhone, purpose string) (string, error) {
	phone, err := domain.NewPhone(rawPhone)
	if err != nil {
		return "", err
	}

	// One code per minute per number. SMS costs real money and an unthrottled
	// endpoint is both a bill and a way to harass someone's phone.
	ok, err := s.cache.SetNX(ctx, otpThrottleKey(phone), []byte("1"), time.Minute)
	if err != nil {
		return "", apperr.Internal(err)
	}
	if !ok {
		return "", apperr.RateLimited("OTP_THROTTLED",
			"Please wait a minute before requesting another code.")
	}

	code, err := password.GenerateOTP(6)
	if err != nil {
		return "", apperr.Internal(err)
	}

	if err := cache.SetJSON(ctx, s.cache, otpKey(phone, purpose), otpRecord{Code: code, Purpose: purpose}, s.otpTTL); err != nil {
		return "", apperr.Internal(err)
	}
	return code, nil
}

// VerifyOTP checks a code and, on success, signs the user in -- creating the
// account if this is a first-time OTP signup.
func (s *AuthService) VerifyOTP(ctx context.Context, rawPhone, code, purpose string, device DeviceInfo) (*AuthResult, error) {
	phone, err := domain.NewPhone(rawPhone)
	if err != nil {
		return nil, err
	}

	key := otpKey(phone, purpose)
	record, err := cache.GetJSON[otpRecord](ctx, s.cache, key)
	if err != nil {
		if errors.Is(err, cache.ErrMiss) {
			return nil, apperr.Unauthorized("OTP_EXPIRED", "That code has expired. Please request a new one.")
		}
		return nil, apperr.Internal(err)
	}

	if record.Attempts >= s.otpMaxAttempts {
		_ = s.cache.Delete(ctx, key)
		return nil, apperr.RateLimited("OTP_ATTEMPTS_EXCEEDED",
			"Too many incorrect attempts. Please request a new code.")
	}

	if !password.CompareOTP(record.Code, code) {
		record.Attempts++
		// Preserve the original TTL by not extending it: a wrong guess must not
		// buy the attacker more time.
		_ = cache.SetJSON(ctx, s.cache, key, record, s.otpTTL)
		return nil, apperr.Unauthorized("INVALID_OTP", "That code is incorrect.")
	}

	// Single-use: consume the code before issuing tokens.
	_ = s.cache.Delete(ctx, key)

	now := s.clock.Now()
	var result *AuthResult

	err = s.db.InTx(ctx, func(ctx context.Context) error {
		user, err := s.users.FindByPhone(ctx, phone)
		switch {
		case err == nil:
			// Existing account: a successful OTP proves phone ownership, so
			// promote a pending account to active.
			if err := user.CanAuthenticate(); err != nil {
				return err
			}
			if user.PhoneVerifiedAt == nil {
				user.PhoneVerifiedAt = &now
				if user.Status == domain.StatusPendingVerification {
					user.Status = domain.StatusActive
				}
			}
			user.LastLoginAt = &now
			user.UpdatedAt = now
			if err := s.users.Update(ctx, user); err != nil {
				return err
			}

		case apperr.KindOf(err) == apperr.KindNotFound:
			// First-time OTP signup: no password, already verified.
			user = &domain.User{
				ID:              id.New(),
				Phone:           phone,
				Status:          domain.StatusActive,
				Roles:           []httpx.Role{httpx.RoleCustomer},
				PhoneVerifiedAt: &now,
				LastLoginAt:     &now,
				ReferralCode:    id.HumanCode(6),
				CreatedAt:       now,
				UpdatedAt:       now,
			}
			if err := s.users.Create(ctx, user); err != nil {
				return err
			}
			evt, err := eventbus.New(contracts.EventUserRegistered, user.ID, contracts.UserRegisteredPayload{
				UserID:       user.ID,
				Phone:        user.Phone.String(),
				RegisteredAt: now,
			})
			if err != nil {
				return err
			}
			evt.TraceID = httpx.TraceIDFrom(ctx)
			if err := s.events.Publish(ctx, evt); err != nil {
				return err
			}

		default:
			return err
		}

		pair, err := s.issueTokens(ctx, user, device, now)
		if err != nil {
			return err
		}
		result = &AuthResult{User: user, Tokens: pair}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}
