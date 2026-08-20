// Package money represents currency amounts as integer minor units (paise).
//
// Floats are never used for money anywhere in this codebase. 0.1+0.2 != 0.3 in
// binary floating point, and a food-delivery ledger that drifts by a paise per
// order becomes unreconcilable at scale. Every amount is an int64 count of the
// currency's smallest unit, and every division is explicit about rounding.
package money

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// Currency is an ISO 4217 code. INR is the only one enabled today; the type
// exists so a second market does not require a schema migration.
type Currency string

const INR Currency = "INR"

// minorUnits is the number of decimal places for a currency.
var minorUnits = map[Currency]int32{INR: 2}

var (
	ErrCurrencyMismatch = errors.New("money: currency mismatch")
	ErrUnknownCurrency  = errors.New("money: unknown currency")
	ErrOverflow         = errors.New("money: arithmetic overflow")
)

// Amount is an immutable currency amount. The zero value is a valid zero INR.
type Amount struct {
	minor    int64
	currency Currency
}

// FromMinor builds an Amount from a raw minor-unit count, e.g. 24999 -> ₹249.99.
// This is the form stored in Postgres (BIGINT) and sent over the wire.
func FromMinor(minor int64, c Currency) Amount {
	if c == "" {
		c = INR
	}
	return Amount{minor: minor, currency: c}
}

// FromRupees is a convenience for literals in code and seed data. It rounds to
// the nearest paise using half-away-from-zero, matching how humans round bills.
func FromRupees(rupees float64) Amount {
	return Amount{minor: int64(math.Round(rupees * 100)), currency: INR}
}

// Parse reads a decimal string like "249.99" without going through float64.
func Parse(s string, c Currency) (Amount, error) {
	if c == "" {
		c = INR
	}
	exp, ok := minorUnits[c]
	if !ok {
		return Amount{}, ErrUnknownCurrency
	}

	s = strings.TrimSpace(s)
	// An empty string must NOT parse as zero. An empty form field or a missing
	// JSON value silently becoming ₹0.00 is how an order gets charged the wrong
	// amount, and it fails silently in exactly the place that matters most.
	if s == "" {
		return Amount{}, fmt.Errorf("money: parse empty string")
	}

	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(strings.TrimPrefix(s, "-"), "+")

	whole, frac, _ := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	// Reject a lone sign or a bare "." for the same reason.
	if whole == "0" && frac == "" && s != "0" {
		return Amount{}, fmt.Errorf("money: parse %q: no digits", s)
	}
	// Pad or truncate the fraction to exactly the currency's precision.
	if int32(len(frac)) > exp {
		frac = frac[:exp]
	}
	frac += strings.Repeat("0", int(exp)-len(frac))

	digits := whole + frac
	minor, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return Amount{}, fmt.Errorf("money: parse %q: %w", s, err)
	}
	if neg {
		minor = -minor
	}
	return Amount{minor: minor, currency: c}, nil
}

func Zero(c Currency) Amount { return Amount{minor: 0, currency: c} }

func (a Amount) Minor() int64       { return a.minor }
func (a Amount) Currency() Currency { return a.normalizedCurrency() }
func (a Amount) IsZero() bool       { return a.minor == 0 }
func (a Amount) IsNegative() bool   { return a.minor < 0 }
func (a Amount) IsPositive() bool   { return a.minor > 0 }

// normalizedCurrency treats the zero value as INR so an uninitialised Amount
// still participates in arithmetic instead of erroring on empty currency.
func (a Amount) normalizedCurrency() Currency {
	if a.currency == "" {
		return INR
	}
	return a.currency
}

// Add returns a+b, erroring if the currencies differ or the result overflows.
func (a Amount) Add(b Amount) (Amount, error) {
	if a.normalizedCurrency() != b.normalizedCurrency() {
		return Amount{}, ErrCurrencyMismatch
	}
	sum := a.minor + b.minor
	// Overflow check: sign of both operands matching but differing from result.
	if (a.minor > 0 && b.minor > 0 && sum < 0) || (a.minor < 0 && b.minor < 0 && sum > 0) {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: sum, currency: a.normalizedCurrency()}, nil
}

func (a Amount) Sub(b Amount) (Amount, error) {
	return a.Add(b.Neg())
}

func (a Amount) Neg() Amount {
	return Amount{minor: -a.minor, currency: a.normalizedCurrency()}
}

// MulInt scales an amount by a whole number, e.g. unit price x quantity.
func (a Amount) MulInt(n int64) (Amount, error) {
	if n != 0 && (a.minor > math.MaxInt64/absInt64(n) || a.minor < math.MinInt64/absInt64(n)) {
		return Amount{}, ErrOverflow
	}
	return Amount{minor: a.minor * n, currency: a.normalizedCurrency()}, nil
}

// ApplyBasisPoints scales by bps/10000, rounding half-up. Used for taxes,
// platform commission and percentage discounts. Basis points avoid the
// float-percentage trap: 5% GST is 500 bps, exactly.
func (a Amount) ApplyBasisPoints(bps int64) Amount {
	product := a.minor * bps
	// Round half away from zero so a 2.5-paise tax becomes 3, not 2.
	if product >= 0 {
		return Amount{minor: (product + 5000) / 10000, currency: a.normalizedCurrency()}
	}
	return Amount{minor: (product - 5000) / 10000, currency: a.normalizedCurrency()}
}

// Clamp bounds an amount to [lo, hi]. Used for "max ₹100 off" style caps.
func (a Amount) Clamp(lo, hi Amount) Amount {
	if a.minor < lo.minor {
		return Amount{minor: lo.minor, currency: a.normalizedCurrency()}
	}
	if a.minor > hi.minor {
		return Amount{minor: hi.minor, currency: a.normalizedCurrency()}
	}
	return a
}

// SplitEvenly divides an amount into n parts whose sum is exactly the original,
// distributing the remainder one paise at a time to the earliest parts. Used for
// splitting a delivery fee or discount across order lines without losing paise.
func (a Amount) SplitEvenly(n int) ([]Amount, error) {
	if n <= 0 {
		return nil, errors.New("money: split count must be positive")
	}
	base := a.minor / int64(n)
	rem := a.minor % int64(n)

	out := make([]Amount, n)
	for i := range out {
		m := base
		if int64(i) < absInt64(rem) {
			if rem > 0 {
				m++
			} else {
				m--
			}
		}
		out[i] = Amount{minor: m, currency: a.normalizedCurrency()}
	}
	return out, nil
}

func (a Amount) Equal(b Amount) bool {
	return a.minor == b.minor && a.normalizedCurrency() == b.normalizedCurrency()
}

// jsonAmount is the wire form of an Amount.
//
// Amount's fields are unexported, which makes it immutable and prevents an
// arbitrary integer being cast into a currency amount. But it also means
// encoding/json cannot see them, so without these methods an Amount silently
// marshals to `{}` and unmarshals to zero.
//
// That failure mode is nasty and worth spelling out: anything that round-trips a
// struct containing an Amount through JSON -- the Redis menu cache, an event
// payload, a queued job -- would return ZERO MONEY. It would not error. A cached
// menu would show every dish as free, and only on a cache hit, so it looks
// intermittent.
type jsonAmount struct {
	Minor    int64    `json:"minor"`
	Currency Currency `json:"currency"`
}

// MarshalJSON encodes minor units plus the currency.
//
// Minor units, never a decimal string or a float: this is the internal wire
// format, and it must survive a round trip with no parsing or rounding at all.
func (a Amount) MarshalJSON() ([]byte, error) {
	return json.Marshal(jsonAmount{Minor: a.minor, Currency: a.normalizedCurrency()})
}

// UnmarshalJSON reconstructs an Amount.
//
// Also accepts a bare integer, so a payload written as `24999` still decodes to
// ₹249.99 rather than failing. Defaults to INR when the currency is absent.
func (a *Amount) UnmarshalJSON(data []byte) error {
	var wire jsonAmount
	if err := json.Unmarshal(data, &wire); err == nil {
		a.minor = wire.Minor
		a.currency = wire.Currency
		if a.currency == "" {
			a.currency = INR
		}
		return nil
	}

	var minor int64
	if err := json.Unmarshal(data, &minor); err != nil {
		return fmt.Errorf("money: unmarshal %s: %w", data, err)
	}
	a.minor = minor
	a.currency = INR
	return nil
}

func (a Amount) GreaterThan(b Amount) bool { return a.minor > b.minor }
func (a Amount) LessThan(b Amount) bool    { return a.minor < b.minor }

// String renders the amount as a plain decimal, e.g. "249.99" -- no symbol, so
// it is safe for APIs and logs. Formatting for display is the frontend's job.
func (a Amount) String() string {
	exp := minorUnits[a.normalizedCurrency()]
	div := int64(math.Pow10(int(exp)))

	sign := ""
	m := a.minor
	if m < 0 {
		sign = "-"
		m = -m
	}
	return fmt.Sprintf("%s%d.%0*d", sign, m/div, exp, m%div)
}

// MustSum adds many amounts, returning an error on the first mismatch. Handy
// for totalling order lines.
func Sum(amounts ...Amount) (Amount, error) {
	if len(amounts) == 0 {
		return Zero(INR), nil
	}
	total := amounts[0]
	for _, a := range amounts[1:] {
		var err error
		if total, err = total.Add(a); err != nil {
			return Amount{}, err
		}
	}
	return total, nil
}

func absInt64(n int64) int64 {
	if n < 0 {
		return -n
	}
	return n
}
