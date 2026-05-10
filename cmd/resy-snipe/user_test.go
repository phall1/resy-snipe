package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// runUserCmd-level tests. Each sub-subcommand is exercised via the
// runUserCmd entry point (not the top-level run() dispatcher — that
// dispatch line is added by the harness at wave end; see user.go's
// header comment for the wiring intent). Tests that need a real DB
// set XDG_DATA_HOME → t.TempDir() so store.Open lands in a per-test
// scratch directory; this matches the pattern in login_test.go.

// TestRunUserCmd_NoSubcommand prints usage and errors. Used to wire
// the binary's "no args" UX so a bare `resy-snipe user` exits non-zero
// with a discoverable message.
func TestRunUserCmd_NoSubcommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), nil, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for missing sub-subcommand")
	}
	got := out.String()
	for _, want := range []string{"Usage:", "seed", "list", "invite", "accept-invite"} {
		if !strings.Contains(got, want) {
			t.Errorf("usage output missing %q: %s", want, got)
		}
	}
}

func TestRunUserCmd_UnknownSubcommand(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"bogus"}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for unknown sub-subcommand")
	}
	if !strings.Contains(err.Error(), "unknown") {
		t.Errorf("error should mention 'unknown': %v", err)
	}
}

func TestRunUserCmd_HelpFlag(t *testing.T) {
	t.Parallel()
	for _, arg := range []string{"-h", "--help", "help"} {
		t.Run(arg, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			err := runUserCmd(context.Background(), []string{arg}, strings.NewReader(""), &out, clock.NewFake(fixedNow))
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", arg, err)
			}
			if !strings.Contains(out.String(), "Usage:") {
				t.Errorf("%s: expected usage output, got %q", arg, out.String())
			}
		})
	}
}

// --- seed -------------------------------------------------------------------

// TestRunUserSeed_WithEmail asserts the happy path: --email <addr>,
// store.SeedOperator runs, and the printed confirmation includes the
// minted UserID + email. We verify the DB row by reopening the same
// path after the command returns.
func TestRunUserSeed_WithEmail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var out bytes.Buffer
	err := runUserCmd(context.Background(),
		[]string{"seed", "--email", "phall@example.com"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runUserCmd seed: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Seeded operator") {
		t.Errorf("output missing 'Seeded operator': %q", got)
	}
	if !strings.Contains(got, "phall@example.com") {
		t.Errorf("output missing email: %q", got)
	}

	// Re-open the DB and confirm exactly one users row with the
	// expected email + role.
	dbPath := dir + "/resy-snipe/db.sqlite"
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()

	var (
		count int
		email string
		role  string
	)
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Errorf("users count = %d, want 1", count)
	}
	if err := db.QueryRowContext(context.Background(),
		`SELECT email, role FROM users`).Scan(&email, &role); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if email != "phall@example.com" {
		t.Errorf("email = %q, want phall@example.com", email)
	}
	if role != "admin" {
		t.Errorf("role = %q, want admin", role)
	}
}

// TestRunUserSeed_PromptsForEmail covers the no-flag interactive
// path: with stdin scripted to deliver "ops@example.com\n", the
// command must consume the line and pass the trimmed email to
// SeedOperator. Verifying the printed confirmation echoes the prompt
// AND the email proves both halves of the prompt round-trip work.
func TestRunUserSeed_PromptsForEmail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	stdin := strings.NewReader("ops@example.com\n")
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"seed"},
		stdin, &out, clock.NewFake(fixedNow))
	if err != nil {
		t.Fatalf("runUserCmd seed (interactive): %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "Operator email") {
		t.Errorf("output missing prompt label: %q", got)
	}
	if !strings.Contains(got, "ops@example.com") {
		t.Errorf("output missing email confirmation: %q", got)
	}
}

// TestRunUserSeed_Idempotent re-runs the seed twice and asserts the
// users row count stays at 1 (and the printed UserID matches across
// runs). Mirrors TestSeedOperatorIsIdempotent on the store side, but
// exercises the path through the CLI handler.
func TestRunUserSeed_Idempotent(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var out1, out2 bytes.Buffer
	for _, b := range []*bytes.Buffer{&out1, &out2} {
		err := runUserCmd(context.Background(),
			[]string{"seed", "--email", "op@example.com"},
			strings.NewReader(""), b, clock.NewFake(fixedNow))
		if err != nil {
			t.Fatalf("runUserCmd seed: %v", err)
		}
	}

	// Both outputs should contain the same UserID. The format is
	// "Seeded operator <uid> (op@example.com); …"; extracting the uid
	// by scanning the prefix is sufficient.
	uid1 := extractSeededUID(t, out1.String())
	uid2 := extractSeededUID(t, out2.String())
	if uid1 != uid2 {
		t.Errorf("idempotent seed returned different UserIDs: %q vs %q", uid1, uid2)
	}

	dbPath := dir + "/resy-snipe/db.sqlite"
	db, err := store.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Errorf("users count after double-seed = %d, want 1", n)
	}
}

func TestRunUserSeed_EmptyEmail(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	// Stdin is empty + no --email flag → promptRaw returns "" and we
	// surface "email is required" without touching the DB.
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"seed"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for empty email")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should mention 'email': %v", err)
	}
}

// TestRunUserSeed_Help confirms `--help` (handled by flag.FlagSet,
// which writes to fs.Output()) lands in our captured writer and
// returns flag.ErrHelp wrapped — our handler surfaces that as a
// non-nil error per the established "any flag parse failure exits
// non-zero" rule.
func TestRunUserSeed_Help(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"seed", "--help"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error from --help (flag.ErrHelp)")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage output, got %q", out.String())
	}
}

// --- list -------------------------------------------------------------------

// TestRunUserList_WithOperator seeds the operator via runUserSeed,
// then runs `user list` against the same DB and asserts the output
// header is present along with one row containing the operator's
// email + role.
func TestRunUserList_WithOperator(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	// Seed first.
	var seedOut bytes.Buffer
	if err := runUserCmd(context.Background(),
		[]string{"seed", "--email", "phall@example.com"},
		strings.NewReader(""), &seedOut, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var listOut bytes.Buffer
	if err := runUserCmd(context.Background(), []string{"list"},
		strings.NewReader(""), &listOut, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("list: %v", err)
	}
	got := listOut.String()
	for _, want := range []string{"ID", "EMAIL", "ROLE", "phall@example.com", "admin"} {
		if !strings.Contains(got, want) {
			t.Errorf("list output missing %q: %q", want, got)
		}
	}
}

// TestRunUserList_Empty asserts the helpful empty-state message when
// no users have been seeded yet.
func TestRunUserList_Empty(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	var out bytes.Buffer
	if err := runUserCmd(context.Background(), []string{"list"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow)); err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out.String(), "no users yet") {
		t.Errorf("expected empty-state message, got %q", out.String())
	}
}

func TestRunUserList_Help(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"list", "--help"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error from --help")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage output, got %q", out.String())
	}
}

// --- invite -----------------------------------------------------------------

func TestRunUserInvite_NotImplemented(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(),
		[]string{"invite", "phall@example.com", "--role=user"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error (Stub returns ErrNotImplemented)")
	}
	if !strings.Contains(err.Error(), "coming in M1-10") {
		t.Errorf("error should mention 'coming in M1-10': %v", err)
	}
	if !strings.Contains(err.Error(), "daemon Service") {
		t.Errorf("error should mention 'daemon Service': %v", err)
	}
}

// TestRunUserInvite_StubErrPath asserts the error wraps
// service.ErrNotImplemented (so any caller doing errors.Is can route
// it). We hit this via the Stub directly to keep the test independent
// of the user-facing copy.
func TestRunUserInvite_StubReturnsNotImplemented(t *testing.T) {
	t.Parallel()
	_, err := (service.Stub{}).InviteUser(context.Background(), "", "x@y.io", "user")
	if !errors.Is(err, service.ErrNotImplemented) {
		t.Errorf("Stub.InviteUser err = %v, want wrap of ErrNotImplemented", err)
	}
}

func TestRunUserInvite_MissingEmail(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"invite"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for missing <email>")
	}
	if !strings.Contains(err.Error(), "email") {
		t.Errorf("error should mention 'email': %v", err)
	}
}

func TestRunUserInvite_BadRole(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(),
		[]string{"invite", "x@y.io", "--role=superuser"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for invalid --role")
	}
	if !strings.Contains(err.Error(), "role") {
		t.Errorf("error should mention 'role': %v", err)
	}
}

func TestRunUserInvite_Help(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"invite", "--help"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error from --help")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage output, got %q", out.String())
	}
}

// --- accept-invite ----------------------------------------------------------

func TestRunUserAcceptInvite_NotImplemented(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(),
		[]string{"accept-invite", "fake-token-abc"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected non-nil error")
	}
	if !strings.Contains(err.Error(), "coming in M1-10") {
		t.Errorf("error should mention 'coming in M1-10': %v", err)
	}
}

func TestRunUserAcceptInvite_MissingToken(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"accept-invite"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error for missing <token>")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("error should mention 'token': %v", err)
	}
}

func TestRunUserAcceptInvite_Help(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	err := runUserCmd(context.Background(), []string{"accept-invite", "--help"},
		strings.NewReader(""), &out, clock.NewFake(fixedNow))
	if err == nil {
		t.Fatal("expected error from --help")
	}
	if !strings.Contains(out.String(), "Usage:") {
		t.Errorf("expected usage output, got %q", out.String())
	}
}

// --- helpers ----------------------------------------------------------------

// extractSeededUID pulls the operator id out of a "Seeded operator
// <uid> (email); …" confirmation line. Returns "" (and fails the test)
// if the prefix is missing.
func extractSeededUID(t *testing.T, s string) string {
	t.Helper()
	const prefix = "Seeded operator "
	_, after, found := strings.Cut(s, prefix)
	if !found {
		t.Fatalf("output missing prefix %q: %s", prefix, s)
	}
	uid, _, found := strings.Cut(after, " ")
	if !found {
		t.Fatalf("output malformed (no space after uid): %s", s)
	}
	return uid
}
