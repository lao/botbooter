// Package asserts holds tiny test assertion helpers shared across botbooter.
package asserts

import "errors"

// TestingT is the subset of *testing.T the assertions use. Accepting the
// interface (which *testing.T satisfies) lets the helpers themselves be
// unit-tested with a recording fake.
type TestingT interface {
	Helper()
	Errorf(format string, args ...any)
}

// Equal fails the test with message if got is not equal to expected.
func Equal[T comparable](t TestingT, got, expected T, message string) {
	t.Helper()
	if got != expected {
		t.Errorf("%s: got %v, expected %v", message, got, expected)
	}
}

// NotNil fails the test with message if got is nil.
func NotNil(t TestingT, got any, message string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected non-nil, got nil", message)
	}
}

// Error fails the test with message if err is nil.
func Error(t TestingT, err error, message string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error, got nil", message)
	}
}

// ErrorIs fails the test with message if err does not match target under errors.Is.
func ErrorIs(t TestingT, err, target error, message string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want errors.Is %v", message, err, target)
	}
}

// NoError fails the test with message if err is non-nil.
func NoError(t TestingT, err error, message string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: expected no error, got %v", message, err)
	}
}

// True fails the test with message if got is false.
func True(t TestingT, got bool, message string) {
	t.Helper()
	if !got {
		t.Errorf("%s: expected true, got false", message)
	}
}

// False fails the test with message if got is true.
func False(t TestingT, got bool, message string) {
	t.Helper()
	if got {
		t.Errorf("%s: expected false, got true", message)
	}
}
