// Package asserts holds tiny test assertion helpers shared across botbooter's
// test packages. It is imported only from _test.go files.
package asserts

import (
	"errors"
	"testing"
)

func Equal[T comparable](t *testing.T, got, expected T, message string) {
	t.Helper()
	if got != expected {
		t.Errorf("%s: got %v, expected %v", message, got, expected)
	}
}

func NotNil(t *testing.T, got any, message string) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: expected non-nil, got nil", message)
	}
}

func Error(t *testing.T, err error, message string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: expected error, got nil", message)
	}
}

func ErrorIs(t *testing.T, err, target error, message string) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Errorf("%s: got %v, want errors.Is %v", message, err, target)
	}
}

func NoError(t *testing.T, err error, message string) {
	t.Helper()
	if err != nil {
		t.Errorf("%s: expected no error, got %v", message, err)
	}
}

func True(t *testing.T, got bool, message string) {
	t.Helper()
	if !got {
		t.Errorf("%s: expected true, got false", message)
	}
}

func False(t *testing.T, got bool, message string) {
	t.Helper()
	if got {
		t.Errorf("%s: expected false, got true", message)
	}
}
