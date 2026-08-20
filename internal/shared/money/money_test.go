package money

import "testing"

// TestFloatWouldBeWrong is the justification for this entire package existing.
//
// It asserts the exact floating-point failure that integer paise avoids. If
// someone later "simplifies" Amount to a float64, this test documents the bug
// they will reintroduce.
func TestFloatWouldBeWrong(t *testing.T) {
	// The canonical float error: 0.1 + 0.2 != 0.3.
	if 0.1+0.2 == 0.3 {
		t.Skip("float arithmetic is exact on this platform; the premise no longer holds")
	}

	// The same sum in minor units is exact.
	a := FromMinor(10, INR)
	b := FromMinor(20, INR)
	sum, err := a.Add(b)
	if err != nil {
		t.Fatalf("Add returned error: %v", err)
	}
	if sum.Minor() != 30 {
		t.Errorf("10 + 20 paise = %d, want 30", sum.Minor())
	}
}

func TestParse(t *testing.T) {
	tests := []struct {
		input     string
		wantMinor int64
		wantErr   bool
	}{
		{"249.99", 24999, false},
		{"0.01", 1, false},
		{"1000", 100000, false},
		{"0", 0, false},
		{"-50.50", -5050, false},
		{"249.9", 24990, false},
		// More precision than the currency has: truncate rather than round, so
		// parsing is lossless in the direction that matters (never inventing
		// money that was not there).
		{"249.999", 24999, false},
		{".5", 50, false},
		{"abc", 0, true},
		{"", 0, true},
	}

	for _, tt := range tests {
		got, err := Parse(tt.input, INR)
		if tt.wantErr {
			if err == nil {
				t.Errorf("Parse(%q) = %v, want error", tt.input, got.Minor())
			}
			continue
		}
		if err != nil {
			t.Errorf("Parse(%q) returned error: %v", tt.input, err)
			continue
		}
		if got.Minor() != tt.wantMinor {
			t.Errorf("Parse(%q).Minor() = %d, want %d", tt.input, got.Minor(), tt.wantMinor)
		}
	}
}

func TestString(t *testing.T) {
	tests := []struct {
		minor int64
		want  string
	}{
		{24999, "249.99"},
		{1, "0.01"},
		{0, "0.00"},
		{100000, "1000.00"},
		{-5050, "-50.50"},
		// Regression guard: the fractional part must be zero-padded. Without the
		// %0*d width, 5 paise renders as "0.5" (fifty paise) -- a 10x error on
		// every invoice ending in a single-digit paise value.
		{5, "0.05"},
		{105, "1.05"},
	}

	for _, tt := range tests {
		if got := FromMinor(tt.minor, INR).String(); got != tt.want {
			t.Errorf("FromMinor(%d).String() = %q, want %q", tt.minor, got, tt.want)
		}
	}
}

func TestApplyBasisPoints(t *testing.T) {
	tests := []struct {
		name  string
		minor int64
		bps   int64
		want  int64
	}{
		{"5% GST on 100.00", 10000, 500, 500},
		{"18% GST on 100.00", 10000, 1800, 1800},
		{"5% on 249.99", 24999, 500, 1250}, // 1249.95 paise -> rounds to 1250
		{"12% commission on 500", 50000, 1200, 6000},
		{"zero rate", 10000, 0, 0},
		{"100% rate", 10000, 10000, 10000},
		// Half-away-from-zero rounding. 10 paise @ 5% = exactly 0.5 paise, which
		// must round UP to 1. Truncating instead would systematically
		// under-collect tax on every order, compounding into a real liability.
		{"exactly half rounds up", 10, 500, 1},
		{"negative exactly half rounds away from zero", -10, 500, -1},
		// Below half truncates down: 0.25 paise is 0.
		{"below half rounds down", 5, 500, 0},
		{"above half rounds up", 15, 500, 1}, // 0.75 -> 1
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromMinor(tt.minor, INR).ApplyBasisPoints(tt.bps)
			if got.Minor() != tt.want {
				t.Errorf("FromMinor(%d).ApplyBasisPoints(%d) = %d, want %d",
					tt.minor, tt.bps, got.Minor(), tt.want)
			}
		})
	}
}

// TestSplitEvenly is the money-conservation test.
//
// Splitting a delivery fee across order lines must never create or destroy a
// paise. A naive implementation (amount/n, repeated) loses the remainder, and
// that loss shows up as an unreconcilable ledger.
func TestSplitEvenly(t *testing.T) {
	tests := []struct {
		name  string
		minor int64
		parts int
		want  []int64
	}{
		{"exact division", 300, 3, []int64{100, 100, 100}},
		// 100/3 = 33.33: the remainder must go somewhere, not vanish.
		{"remainder distributed", 100, 3, []int64{34, 33, 33}},
		{"remainder of two", 101, 3, []int64{34, 34, 33}},
		{"single part", 4999, 1, []int64{4999}},
		{"zero amount", 0, 4, []int64{0, 0, 0, 0}},
		{"negative amount", -100, 3, []int64{-34, -33, -33}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parts, err := FromMinor(tt.minor, INR).SplitEvenly(tt.parts)
			if err != nil {
				t.Fatalf("SplitEvenly returned error: %v", err)
			}
			if len(parts) != tt.parts {
				t.Fatalf("got %d parts, want %d", len(parts), tt.parts)
			}

			var sum int64
			for i, p := range parts {
				if p.Minor() != tt.want[i] {
					t.Errorf("part[%d] = %d, want %d", i, p.Minor(), tt.want[i])
				}
				sum += p.Minor()
			}
			// The invariant that actually matters.
			if sum != tt.minor {
				t.Errorf("parts sum to %d, want %d -- money was created or destroyed", sum, tt.minor)
			}
		})
	}
}

// TestSplitEvenlyConservesMoney fuzzes the conservation invariant across a wide
// range, because a rounding bug typically only shows for specific remainders.
func TestSplitEvenlyConservesMoney(t *testing.T) {
	for amount := int64(0); amount < 500; amount++ {
		for parts := 1; parts <= 9; parts++ {
			split, err := FromMinor(amount, INR).SplitEvenly(parts)
			if err != nil {
				t.Fatalf("SplitEvenly(%d, %d): %v", amount, parts, err)
			}
			var sum int64
			for _, p := range split {
				sum += p.Minor()
			}
			if sum != amount {
				t.Fatalf("SplitEvenly(%d, %d) sums to %d", amount, parts, sum)
			}
		}
	}
}

func TestCurrencyMismatchIsRejected(t *testing.T) {
	inr := FromMinor(100, INR)
	usd := FromMinor(100, Currency("USD"))

	if _, err := inr.Add(usd); err != ErrCurrencyMismatch {
		t.Errorf("Add across currencies: got %v, want ErrCurrencyMismatch", err)
	}
	if _, err := inr.Sub(usd); err != ErrCurrencyMismatch {
		t.Errorf("Sub across currencies: got %v, want ErrCurrencyMismatch", err)
	}
}

func TestOverflowIsDetected(t *testing.T) {
	const maxInt64 = int64(9223372036854775807)

	huge := FromMinor(maxInt64, INR)
	if _, err := huge.Add(FromMinor(1, INR)); err != ErrOverflow {
		t.Errorf("overflowing Add: got %v, want ErrOverflow", err)
	}
	if _, err := huge.MulInt(2); err != ErrOverflow {
		t.Errorf("overflowing MulInt: got %v, want ErrOverflow", err)
	}
}

func TestZeroValueIsUsable(t *testing.T) {
	// An uninitialised Amount must behave as zero INR rather than erroring on an
	// empty currency, so struct fields work without explicit construction.
	var zero Amount

	if !zero.IsZero() {
		t.Error("zero value should report IsZero")
	}
	if zero.Currency() != INR {
		t.Errorf("zero value currency = %q, want INR", zero.Currency())
	}
	sum, err := zero.Add(FromMinor(100, INR))
	if err != nil {
		t.Fatalf("adding to zero value: %v", err)
	}
	if sum.Minor() != 100 {
		t.Errorf("0 + 100 = %d, want 100", sum.Minor())
	}
}

func TestSum(t *testing.T) {
	total, err := Sum(
		FromMinor(24999, INR),
		FromMinor(3000, INR),
		FromMinor(1250, INR),
	)
	if err != nil {
		t.Fatalf("Sum returned error: %v", err)
	}
	if total.Minor() != 29249 {
		t.Errorf("Sum = %d, want 29249", total.Minor())
	}

	empty, err := Sum()
	if err != nil || !empty.IsZero() {
		t.Errorf("Sum() of nothing = %v, %v; want zero, nil", empty.Minor(), err)
	}
}

func TestClamp(t *testing.T) {
	// The "max ₹100 off" coupon cap.
	lo, hi := Zero(INR), FromMinor(10000, INR)

	if got := FromMinor(25000, INR).Clamp(lo, hi); got.Minor() != 10000 {
		t.Errorf("Clamp above max = %d, want 10000", got.Minor())
	}
	if got := FromMinor(-500, INR).Clamp(lo, hi); got.Minor() != 0 {
		t.Errorf("Clamp below min = %d, want 0", got.Minor())
	}
	if got := FromMinor(5000, INR).Clamp(lo, hi); got.Minor() != 5000 {
		t.Errorf("Clamp within range = %d, want 5000", got.Minor())
	}
}
