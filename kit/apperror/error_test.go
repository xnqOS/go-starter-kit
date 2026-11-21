package apperror

import (
	"errors"
	"testing"
)

func TestErrorKeepsKindForLogs(t *testing.T) {
	err := New(ErrUnavailable, "payment_down", "try later")
	if got := err.Error(); got != "unavailable: payment_down: try later" {
		t.Fatalf("Error() = %q", got)
	}
	if !errors.Is(err, ErrUnavailable) || err.Kind() != ErrUnavailable {
		t.Fatal("kind lost")
	}
	invalid := Invalid("name is required")
	if invalid.Code != "" || !errors.Is(invalid, ErrInvalid) || invalid.Error() != "invalid input: name is required" {
		t.Fatalf("Invalid() = %+v", invalid)
	}
}
