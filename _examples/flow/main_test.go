package main

import "testing"

// validEmail is the illustrative step validator wired into the signup flow. It
// only checks for an "@", so the cases below pin that deliberately-minimal
// contract: anything with an "@" passes, anything without is re-prompted.
func TestValidEmail(t *testing.T) {
	cases := []struct {
		name string
		in   string
		ok   bool
	}{
		{"Typical", "user@example.com", true},
		{"BareAtSign", "@", true}, // documents the known limitation of the simple check
		{"NoAtSign", "plainaddress", false},
		{"Empty", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validEmail(tc.in)
			if (err == nil) != tc.ok {
				t.Errorf("validEmail(%q) error = %v, want ok=%v", tc.in, err, tc.ok)
			}
		})
	}
}
