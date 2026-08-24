package app_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/zevoxa/chowk-backend/internal/config"
	identitypg "github.com/zevoxa/chowk-backend/internal/modules/identity/adapter/postgres"
	"github.com/zevoxa/chowk-backend/internal/modules/identity/app"
	"github.com/zevoxa/chowk-backend/internal/platform/password"
	"github.com/zevoxa/chowk-backend/internal/platform/token"
	"github.com/zevoxa/chowk-backend/internal/shared/apperr"
	"github.com/zevoxa/chowk-backend/internal/shared/clock"
	"github.com/zevoxa/chowk-backend/internal/shared/id"
	"github.com/zevoxa/chowk-backend/internal/testutil"
)

/*
identity/app had ZERO tests across 552 lines of authentication.

Register, Login, Refresh, Logout, LogoutEverywhere, RequestOTP and VerifyOTP were
all uncovered — the code that decides who is allowed to be whom. The comments in
auth.go describe several deliberate security properties, and a property nothing
asserts is a property the next refactor removes silently.

Against real Postgres and real Redis, because the session rotation chain and the
OTP attempt counter live there.

bcrypt cost is deliberately the minimum here. The production default of 12 is
~250ms per hash, and these tests hash often enough that the default would make the
suite unpleasant to run. Cost does not affect any behaviour asserted below.
*/

const testCost = 4

type authFixture struct {
	svc      *app.AuthService
	sessions *identitypg.SessionRepository
	phone    string
	password string
}

func newAuth(t *testing.T) *authFixture {
	t.Helper()
	db := testutil.DB(t)
	cache := testutil.Cache(t)

	users := identitypg.NewUserRepository(db)
	sessions := identitypg.NewSessionRepository(db)

	tokens := token.NewManager(config.AuthConfig{
		JWTSecret:       "test-secret-that-is-comfortably-long-enough-for-hmac",
		AccessTokenTTL:  15 * time.Minute,
		RefreshTokenTTL: 720 * time.Hour,
	})

	f := &authFixture{
		sessions: sessions,
		// Unique per test so the phone unique index cannot collide across runs.
		phone:    "+9199" + digits(id.New()),
		password: "CorrectHorseBatteryStaple9!",
	}
	f.svc = app.NewAuthService(
		db, users, sessions, tokens, password.NewHasher(testCost),
		cache, testutil.NewRecordingPublisher(), clock.System{},
		5*time.Minute, 5,
	)

	t.Cleanup(func() {
		c := context.Background()
		_, _ = db.Q(c).Exec(c, `DELETE FROM sessions WHERE user_id IN (SELECT id FROM users WHERE phone = $1)`, f.phone)
		_, _ = db.Q(c).Exec(c, `DELETE FROM users WHERE phone = $1`, f.phone)
	})
	return f
}

// digits renders 8 decimal digits unique per call.
//
// FROM THE END of the uuid. id.New() is UUIDv7 and leads with a millisecond
// timestamp, so consecutive ids share their leading characters — verified: five ids
// in a row produced the identical 8-digit prefix. Taking the front made the
// "nonexistent" phone below equal to the registered one, and the test reported an
// authentication bypass that did not exist.
func digits(from id.ID) string {
	raw := from.String()
	out := make([]byte, 0, 8)
	for i := len(raw) - 1; i >= 0 && len(out) < 8; i-- {
		if raw[i] >= '0' && raw[i] <= '9' {
			out = append(out, raw[i])
		}
	}
	for len(out) < 8 {
		out = append(out, '0')
	}
	return string(out)
}

func (f *authFixture) register(t *testing.T) *app.AuthResult {
	t.Helper()
	res, err := f.svc.Register(context.Background(), app.RegisterInput{
		Phone: f.phone, FullName: "Test Person", Password: f.password,
		Device: app.DeviceInfo{UserAgent: "test", IPAddress: "127.0.0.1", DeviceName: "test-device"},
	})
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	return res
}

// --------------------------------------------------------------------------
// Login: enumeration resistance
// --------------------------------------------------------------------------

func TestLoginRevealsNothingAboutWhichPhonesExist(t *testing.T) {
	/*
		auth.go promises: "Every failure path returns the same INVALID_CREDENTIALS
		error and burns comparable CPU, so an attacker cannot use the response to
		learn which phone numbers are registered."

		Both halves are asserted. The error text is the easy half; the TIMING is the
		one a refactor breaks, by returning early when the user is not found instead
		of comparing against a dummy hash. That turns the login endpoint into a
		registered-user oracle, which is a privacy leak on a food app and a
		credential-stuffing shortlist anywhere.
	*/
	f := newAuth(t)
	f.register(t)
	ctx := context.Background()

	unknownPhone := "+9199" + digits(id.New())

	wrongPassword := func() error {
		_, err := f.svc.Login(ctx, app.LoginInput{Phone: f.phone, Password: "definitely-not-it"})
		return err
	}
	noSuchUser := func() error {
		_, err := f.svc.Login(ctx, app.LoginInput{Phone: unknownPhone, Password: f.password})
		return err
	}

	errWrong, errUnknown := wrongPassword(), noSuchUser()
	if errWrong == nil {
		t.Fatal("LOGIN WITH THE WRONG PASSWORD SUCCEEDED")
	}
	if errUnknown == nil {
		t.Fatalf("LOGIN FOR A NONEXISTENT PHONE (%s) SUCCEEDED", unknownPhone)
	}
	if apperr.CodeOf(errWrong) != apperr.CodeOf(errUnknown) {
		t.Errorf("different codes for wrong-password (%s) and no-such-user (%s): the "+
			"response tells an attacker which phones are registered",
			apperr.CodeOf(errWrong), apperr.CodeOf(errUnknown))
	}
	if errWrong.Error() != errUnknown.Error() {
		t.Errorf("different messages:\n  wrong password: %v\n  no such user:   %v", errWrong, errUnknown)
	}

	// Timing. Measured as a ratio rather than an absolute, and generously, because
	// this runs on shared CI hardware. An early return would make the no-such-user
	// path faster by roughly the cost of a bcrypt comparison — an order of
	// magnitude, not 3x.
	median := func(fn func() error) time.Duration {
		var samples []time.Duration
		for i := 0; i < 5; i++ {
			start := time.Now()
			_ = fn()
			samples = append(samples, time.Since(start))
		}
		for i := range samples {
			for j := i + 1; j < len(samples); j++ {
				if samples[j] < samples[i] {
					samples[i], samples[j] = samples[j], samples[i]
				}
			}
		}
		return samples[len(samples)/2]
	}

	withUser, withoutUser := median(wrongPassword), median(noSuchUser)
	if withoutUser*3 < withUser {
		t.Errorf("no-such-user took %v but wrong-password took %v — the missing-user "+
			"path is skipping the hash comparison, making login an enumeration oracle",
			withoutUser, withUser)
	}
}

// --------------------------------------------------------------------------
// Refresh: rotation and reuse detection
// --------------------------------------------------------------------------

func TestRefreshRotatesTheTokenSoItIsSingleUse(t *testing.T) {
	f := newAuth(t)
	first := f.register(t)
	ctx := context.Background()

	second, err := f.svc.Refresh(ctx, first.Tokens.RefreshToken, app.DeviceInfo{})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if second.Tokens.RefreshToken == first.Tokens.RefreshToken {
		t.Fatal("the refresh token was reused rather than rotated; a token captured " +
			"once would then work forever")
	}
	if second.Tokens.AccessToken == first.Tokens.AccessToken {
		t.Error("the access token was not reissued")
	}
}

func TestReusingARotatedRefreshTokenRevokesEverySession(t *testing.T) {
	/*
		The property worth the most, from auth.go: "If a token that was already
		rotated is presented again, either the network duplicated it or it was stolen
		— and since we cannot tell, every session for that user is revoked. That
		turns a stolen token into one extra login instead of permanent silent
		access."

		The revocation is the point. Rejecting the replay alone would leave the thief
		holding whatever they rotated to.
	*/
	f := newAuth(t)
	first := f.register(t)
	ctx := context.Background()

	// The attacker's exchange succeeds — they hold a valid pair now.
	stolen, err := f.svc.Refresh(ctx, first.Tokens.RefreshToken, app.DeviceInfo{})
	if err != nil {
		t.Fatalf("first refresh: %v", err)
	}

	// The real user then presents the token they still have, which was rotated.
	_, err = f.svc.Refresh(ctx, first.Tokens.RefreshToken, app.DeviceInfo{})
	if err == nil {
		t.Fatal("an already-rotated refresh token was accepted a second time")
	}
	if code := apperr.CodeOf(err); code != "TOKEN_REUSE_DETECTED" {
		t.Errorf("code = %q, want TOKEN_REUSE_DETECTED", code)
	}

	// And the thief's token must now be dead too.
	if _, err := f.svc.Refresh(ctx, stolen.Tokens.RefreshToken, app.DeviceInfo{}); err == nil {
		t.Error("the rotated-to token still works after reuse was detected; the " +
			"attacker keeps their access, which defeats the whole mechanism")
	}

	active, err := f.sessions.ListActiveForUser(ctx, first.User.ID, time.Now())
	if err != nil {
		t.Fatalf("list sessions: %v", err)
	}
	if len(active) != 0 {
		t.Errorf("%d sessions still active after reuse detection, want 0", len(active))
	}
}

func TestRefreshRejectsGarbageWithoutLeakingWhy(t *testing.T) {
	f := newAuth(t)
	ctx := context.Background()

	for name, tok := range map[string]string{
		"empty":     "",
		"garbage":   "not-a-token",
		"plausible": strings.Repeat("a", 64),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := f.svc.Refresh(ctx, tok, app.DeviceInfo{}); err == nil {
				t.Fatal("accepted an invalid refresh token")
			}
		})
	}
}

// --------------------------------------------------------------------------
// Logout scope
// --------------------------------------------------------------------------

func TestLogoutEndsOneDeviceAndLogoutEverywhereEndsAll(t *testing.T) {
	f := newAuth(t)
	first := f.register(t)
	ctx := context.Background()

	// A second device.
	second, err := f.svc.Login(ctx, app.LoginInput{
		Phone: f.phone, Password: f.password,
		Device: app.DeviceInfo{DeviceName: "phone-2"},
	})
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	before, err := f.sessions.ListActiveForUser(ctx, first.User.ID, time.Now())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(before) < 2 {
		t.Fatalf("expected 2 active sessions, got %d", len(before))
	}

	// AuthResult deliberately does not expose the session, so the id comes from the
	// repository — which is also how the HTTP layer gets it, from the access token's
	// claims.
	if err := f.svc.Logout(ctx, before[0].ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	mid, err := f.sessions.ListActiveForUser(ctx, first.User.ID, time.Now())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(mid) != len(before)-1 {
		t.Errorf("after one logout there are %d sessions, want %d", len(mid), len(before)-1)
	}

	if err := f.svc.LogoutEverywhere(ctx, first.User.ID); err != nil {
		t.Fatalf("logout everywhere: %v", err)
	}
	after, err := f.sessions.ListActiveForUser(ctx, first.User.ID, time.Now())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != 0 {
		t.Errorf("%d sessions survived LogoutEverywhere, want 0", len(after))
	}
	_ = second
}

// --------------------------------------------------------------------------
// Registration
// --------------------------------------------------------------------------

func TestRegisterRejectsADuplicatePhone(t *testing.T) {
	f := newAuth(t)
	f.register(t)
	ctx := context.Background()

	_, err := f.svc.Register(ctx, app.RegisterInput{
		Phone: f.phone, FullName: "Someone Else", Password: "AnotherPassword9!",
	})
	if err == nil {
		t.Fatal("a second account was created on the same phone number, which is how " +
			"one person ends up able to sign in as another")
	}
	if k := apperr.KindOf(err); k != apperr.KindConflict && k != apperr.KindInvalid {
		t.Errorf("kind = %v, want Conflict or Invalid", k)
	}
}

func TestRegisterStoresAHashNeverThePassword(t *testing.T) {
	f := newAuth(t)
	res := f.register(t)
	ctx := context.Background()
	db := testutil.DB(t)

	var stored string
	if err := db.Q(ctx).QueryRow(ctx,
		`SELECT coalesce(password_hash,'') FROM users WHERE id = $1`, res.User.ID).Scan(&stored); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if stored == "" {
		t.Fatal("no password hash was stored, so this account cannot log in")
	}
	if strings.Contains(stored, f.password) {
		t.Fatal("the plaintext password appears in the stored value")
	}
	// bcrypt hashes are self-describing; anything else means the hasher was swapped
	// for something reversible.
	if !strings.HasPrefix(stored, "$2") {
		t.Errorf("stored credential %q is not a bcrypt hash", stored[:min(8, len(stored))])
	}
}

// --------------------------------------------------------------------------
// OTP
// --------------------------------------------------------------------------

func TestOTPVerifiesOnceAndRejectsAWrongCode(t *testing.T) {
	f := newAuth(t)
	ctx := context.Background()

	code, err := f.svc.RequestOTP(ctx, f.phone, "login")
	if err != nil {
		t.Fatalf("request otp: %v", err)
	}
	if code == "" {
		t.Fatal("no OTP was issued")
	}

	if _, err := f.svc.VerifyOTP(ctx, f.phone, "000000", "login", app.DeviceInfo{}); err == nil {
		t.Error("a wrong OTP was accepted")
	}

	res, err := f.svc.VerifyOTP(ctx, f.phone, code, "login", app.DeviceInfo{})
	if err != nil {
		t.Fatalf("verify correct otp: %v", err)
	}
	if res.Tokens.AccessToken == "" {
		t.Error("verification issued no access token")
	}

	// Single use. A reusable OTP is a permanent password sitting in somebody's SMS
	// history.
	if _, err := f.svc.VerifyOTP(ctx, f.phone, code, "login", app.DeviceInfo{}); err == nil {
		t.Error("the same OTP was accepted twice")
	}
}

func TestOTPGivesUpAfterTooManyAttempts(t *testing.T) {
	f := newAuth(t)
	ctx := context.Background()

	code, err := f.svc.RequestOTP(ctx, f.phone, "login")
	if err != nil {
		t.Fatalf("request otp: %v", err)
	}

	// The fixture allows 5. Burn them all on wrong guesses.
	for i := 0; i < 5; i++ {
		if _, err := f.svc.VerifyOTP(ctx, f.phone, "000000", "login", app.DeviceInfo{}); err == nil {
			t.Fatalf("attempt %d: a wrong OTP was accepted", i+1)
		}
	}

	// Now even the RIGHT code must fail. Without this, a 6-digit code is brute
	// forceable in a million requests, which is minutes of scripted traffic.
	if _, err := f.svc.VerifyOTP(ctx, f.phone, code, "login", app.DeviceInfo{}); err == nil {
		t.Error("the correct OTP still worked after the attempt limit was exhausted, " +
			"so the limit does not limit anything")
	}
}
