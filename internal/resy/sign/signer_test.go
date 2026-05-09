package sign_test

import (
	"context"
	"testing"

	"resy-snipe/internal/resy/sign"
)

func TestNoopReturnsNoHeaders(t *testing.T) {
	t.Parallel()
	var s sign.Noop
	h, err := s.Sign(context.Background(), "/3/details")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if len(h) != 0 {
		t.Errorf("Noop.Sign returned %d headers, want 0", len(h))
	}
}

func TestNoopResetIsNoOp(t *testing.T) {
	t.Parallel()
	var s sign.Noop
	if err := s.Reset(context.Background()); err != nil {
		t.Errorf("Noop.Reset: %v", err)
	}
	// Calling Reset twice must remain a no-op.
	if err := s.Reset(context.Background()); err != nil {
		t.Errorf("second Reset: %v", err)
	}
}
