package service

import (
	"context"
	"errors"
	"testing"

	"resy-snipe/internal/domain"
)

func TestStubReturnsNotImplemented(t *testing.T) {
	t.Parallel()

	var s Service = Stub{}
	ctx := context.Background()
	uid := domain.UserID("u-test")
	qid := domain.QuestID("q-test")

	if _, err := s.ResolveVenue(ctx, uid, domain.VenueQuerySlug{Slug: "x", City: "y"}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ResolveVenue: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.PlanQuest(ctx, uid, domain.Goal{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("PlanQuest: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.CreateQuest(ctx, uid, domain.Goal{}, CreateOpts{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("CreateQuest: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.GetQuest(ctx, uid, qid); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("GetQuest: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.ListQuests(ctx, uid, ListFilter{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ListQuests: want ErrNotImplemented, got %v", err)
	}
	if err := s.CancelQuest(ctx, uid, qid, CancelOpts{}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("CancelQuest: want ErrNotImplemented, got %v", err)
	}
	if err := s.SubscribeQuest(ctx, uid, qid, func(domain.Event) {}); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("SubscribeQuest: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.Login(ctx, uid, "a@b.c", "pw"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("Login: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.ListAccounts(ctx, uid); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ListAccounts: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.InviteUser(ctx, uid, "a@b.c", "tenant"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("InviteUser: want ErrNotImplemented, got %v", err)
	}
	if _, _, err := s.AcceptInvite(ctx, "tok", "a@b.c", "pw"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("AcceptInvite: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.IssueToken(ctx, uid, "cli", "user"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("IssueToken: want ErrNotImplemented, got %v", err)
	}
	if err := s.RevokeToken(ctx, uid, "t-1"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("RevokeToken: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.ListTokens(ctx, uid); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ListTokens: want ErrNotImplemented, got %v", err)
	}
	if _, err := s.ListUsers(ctx, uid); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("ListUsers: want ErrNotImplemented, got %v", err)
	}
}

func TestSentinelsAreDistinct(t *testing.T) {
	t.Parallel()

	sentinels := map[string]error{
		"ErrNotImplemented":      ErrNotImplemented,
		"ErrNotFound":            ErrNotFound,
		"ErrUnauthorized":        ErrUnauthorized,
		"ErrForbidden":           ErrForbidden,
		"ErrConflict":            ErrConflict,
		"ErrInvalidPlanHash":     ErrInvalidPlanHash,
		"ErrVenueNotFound":       ErrVenueNotFound,
		"ErrVenueAmbiguous":      ErrVenueAmbiguous,
		"ErrUpstreamUnavailable": ErrUpstreamUnavailable,
		"ErrInvalidArgument":     ErrInvalidArgument,
		"ErrTokenExpired":        ErrTokenExpired,
		"ErrTokenRevoked":        ErrTokenRevoked,
		"ErrInviteExpired":       ErrInviteExpired,
		"ErrInviteConsumed":      ErrInviteConsumed,
		"ErrIdempotencyConflict": ErrIdempotencyConflict,
	}

	for nameA, a := range sentinels {
		for nameB, b := range sentinels {
			if nameA == nameB {
				continue
			}
			if errors.Is(a, b) {
				t.Errorf("sentinel %s should not match %s", nameA, nameB)
			}
		}
	}
}

func TestIsHelpers(t *testing.T) {
	t.Parallel()

	if !IsNotFound(ErrNotFound) {
		t.Error("IsNotFound(ErrNotFound) = false, want true")
	}
	if IsNotFound(ErrConflict) {
		t.Error("IsNotFound(ErrConflict) = true, want false")
	}
	wrapped := errors.Join(errors.New("ctx"), ErrUnauthorized)
	if !IsUnauthorized(wrapped) {
		t.Error("IsUnauthorized(wrapped) = false, want true")
	}
	if !IsConflict(ErrConflict) {
		t.Error("IsConflict(ErrConflict) = false, want true")
	}
	if !IsInvalidArgument(ErrInvalidArgument) {
		t.Error("IsInvalidArgument(ErrInvalidArgument) = false, want true")
	}
}
