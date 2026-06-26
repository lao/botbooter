// Package asserts holds tiny test assertion helpers shared across botbooter's
// test packages. It is imported only from _test.go files.
package asserts

import (
	"errors"
	"testing"
)

// Equal fails the test with message if got is not equal to expected.
func Equal[T comparable](t *testing.T, got, expected T, message string) {
	t.Helper()
	if got != expected {
		t.Errorf("%s: got %v, expected %v", message, got, expected)
	}
}

// NotNil fails the test with message if got is nil.
func NotNil(t *testing.T, got any, message string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected non-nil, got nil", message)
	}
}

// Error fails the test with message if err is nil.
func Error(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error, got nil", message)
	}
}

// ErrorIs fails the test with message if err does not match target under
// errors.Is.
func ErrorIs(t *testing.T, err, target error, message string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want errors.Is %v", message, err, target)
	}
}

// NoError fails the test with message if err is non-nil.
func NoError(t *testing.T, err error, message string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: expected no error, got %v", message, err)
	}
}

// True fails the test with message if got is false.
func True(t *testing.T, got bool, message string) {
	t.Helper()
	if !got {
		t.Errorf("%s: expected true, got false", message)
	}
}

// False fails the test with message if got is true.
func False(t *testing.T, got bool, message string) {
	t.Helper()
	if got {
		t.Errorf("%s: expected false, got true", message)
	}
}
