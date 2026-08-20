// Package clock abstracts "now".
//
// Domain logic that calls time.Now() directly is untestable: you cannot assert
// that a coupon expires, a kitchen closes at 23:00, or an OTP times out without
// sleeping in the test. Every service takes a Clock instead.
package clock

import "time"

type Clock interface {
	Now() time.Time
}

// System is the production clock. It returns UTC unconditionally -- all
// timestamps are stored and compared in UTC, and converted to Asia/Kolkata only
// at the presentation edge.
type System struct{}

func (System) Now() time.Time { return time.Now().UTC() }

// Fixed is a deterministic clock for tests.
type Fixed struct{ T time.Time }

func (f *Fixed) Now() time.Time { return f.T }

// Advance moves a Fixed clock forward, letting a test step past an expiry.
func (f *Fixed) Advance(d time.Duration) { f.T = f.T.Add(d) }

// IST is the display timezone for this business. Loaded once at init; falls back
// to a fixed +05:30 offset if the system tzdata is unavailable (common in
// scratch containers).
var IST = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Kolkata"); err == nil {
		return loc
	}
	return time.FixedZone("IST", 5*3600+1800)
}()
