package domain

import "testing"

// TestNewPhone pins the normalisation rules. The 91-prefix cases are the reason
// this test exists: an earlier version stripped a leading "91" unconditionally,
// which corrupted valid numbers that merely start with 91 into 8-digit garbage
// and rejected real customers at signup.
func TestNewPhone(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  Phone
		valid bool
	}{
		// Accepted forms -- all the ways a human types the same number.
		{"plain 10 digit", "9876543210", "+919876543210", true},
		{"with country code", "+919876543210", "+919876543210", true},
		{"country code no plus", "919876543210", "+919876543210", true},
		{"leading zero", "09876543210", "+919876543210", true},
		{"spaces", "98765 43210", "+919876543210", true},
		{"dashes and brackets", "(98765)-43210", "+919876543210", true},
		{"spaced country code", "+91 98765 43210", "+919876543210", true},

		// The regression this test was written for: valid mobiles that begin with
		// the digits 91 must NOT have those digits treated as a country code.
		{"number starting 91", "9123456780", "+919123456780", true},
		{"number starting 91 with cc", "919123456780", "+919123456780", true},
		{"number starting 91 with plus cc", "+919123456780", "+919123456780", true},

		// Rejected.
		{"too short", "12345", "", false},
		{"nine digits", "987654321", "", false},
		{"eleven digits", "98765432101", "", false},
		{"starts with 5", "5876543210", "", false},
		{"starts with 0 after strip", "0123456789", "", false},
		{"empty", "", "", false},
		{"letters only", "abcdefghij", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewPhone(tt.input)

			if !tt.valid {
				if err == nil {
					t.Fatalf("NewPhone(%q) = %q, want error", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NewPhone(%q) returned error: %v", tt.input, err)
			}
			if got != tt.want {
				t.Errorf("NewPhone(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

// TestPhoneMasked guards the privacy helper: full phone numbers must never reach
// logs or support UIs.
func TestPhoneMasked(t *testing.T) {
	p := Phone("+919876543210")
	if got, want := p.Masked(), "+91XXXXXX3210"; got != want {
		t.Errorf("Masked() = %q, want %q", got, want)
	}
	if got, want := p.National(), "9876543210"; got != want {
		t.Errorf("National() = %q, want %q", got, want)
	}
}
