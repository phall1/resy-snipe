// Package main: `resy-snipe user …` subcommand.
//
// Wiring intent: this file is wired into main.go's subcommand dispatch
// by adding the following single branch near the existing `login`
// branch (the harness performs the edit at wave end; see beads issue
// resy-snipe-rld / M1-21):
//
//	if len(args) > 0 && args[0] == "user" {
//	    return runUserCmd(context.Background(), args[1:], stdin, logOut, clk)
//	}
//
// Until that line lands the entry point below is reachable only via
// the cmd/resy-snipe test suite (which calls runUserCmd directly).
//
// Sub-subcommands:
//
//   - seed [--email <email>]   mint the operator + backfill v1
//     accounts. Works today against
//     store.SeedOperator directly because the
//     Service layer's Login/Account flows aren't
//     wired yet.
//
//   - list                     print every homelab user. Reads
//     directly from the users table via the
//     cross-tenant admin function store.ListUsers
//     (documented exemption in tenant_allowlist.go).
//     Refactor target when M1-10 lands: replace
//     with Service.ListUsers.
//
//   - invite <email> [--role=…]  calls Service.InviteUser. Today
//     surfaces ErrNotImplemented as a friendly
//     "coming in M1-10" message; non-zero exit.
//
//   - accept-invite <token>    calls Service.AcceptInvite. Same
//     deferred-impl treatment as invite.
//
// Per Law 4 the CLI handler is wiring only — password hashing, token
// minting, invite issuance, and role-gated admin checks all stay
// inside internal/service and internal/secrets.
package main

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
	"resy-snipe/internal/service"
	"resy-snipe/internal/store"
)

// runUserCmd is the top-level entry for `resy-snipe user …`. It
// dispatches on args[0] to the per-sub-sub handler. The signature
// mirrors runLogin so main.go's dispatch (when added) can call it
// without ceremony.
//
// `args` is everything after the `user` token — e.g. for
// `resy-snipe user seed --email foo@bar`, args = ["seed", "--email",
// "foo@bar"]. An empty args slice prints the user-subcommand usage and
// returns a non-nil error so the binary exits non-zero, matching the
// rest of the CLI's "unrecognized invocation = error" convention.
func runUserCmd(ctx context.Context, args []string, stdin io.Reader, out io.Writer, clk clock.Clock) error {
	if len(args) == 0 {
		printUserUsage(out)
		return errors.New("user: missing sub-subcommand (want one of: seed, list, invite, accept-invite)")
	}
	switch args[0] {
	case "seed":
		return runUserSeed(ctx, args[1:], stdin, out, clk)
	case "list":
		return runUserList(ctx, args[1:], out, clk)
	case "invite":
		return runUserInvite(ctx, args[1:], out)
	case "accept-invite":
		return runUserAcceptInvite(ctx, args[1:], out)
	case "-h", "--help", "help":
		printUserUsage(out)
		return nil
	default:
		printUserUsage(out)
		return fmt.Errorf("user: unknown sub-subcommand %q", args[0])
	}
}

// printUserUsage writes the top-level `user` help to out. Format
// mirrors flag.FlagSet.PrintDefaults so the look matches the rest of
// the CLI.
func printUserUsage(out io.Writer) {
	fprintln(out, "Usage: resy-snipe user <subcommand> [flags]")
	fprintln(out, "")
	fprintln(out, "Subcommands:")
	fprintln(out, "  seed           Mint the operator (first-run admin) and bind v1 accounts.")
	fprintln(out, "  list           List every homelab user.")
	fprintln(out, "  invite         Issue an invite for a new tenant (requires daemon Service; coming in M1-10).")
	fprintln(out, "  accept-invite  Redeem an invite token (requires daemon Service; coming in M1-10).")
}

// notImplementedMsg is the friendly copy surfaced by `user invite` and
// `user accept-invite` while the Service layer is still the Stub. The
// shape ("requires the daemon Service layer (coming in M1-10)") is
// deliberately load-bearing — the test suite asserts on substrings so
// rewording it requires updating user_test.go too.
const notImplementedMsg = "requires the daemon Service layer (coming in M1-10). " +
	"For now, use 'user seed' to create the operator and direct DB operations for additional users."

// runUserSeed implements `user seed [--email <email>]`.
//
// The seed flow is two SQL writes wrapped by the existing
// store.SeedOperator + store.BackfillOperatorAccounts helpers:
//   - SeedOperator mints the single admin row (idempotent: re-runs
//     with the same email return the existing UserID).
//   - BackfillOperatorAccounts attaches every v1-imported `accounts`
//     row (user_id IS NULL) to the freshly-minted operator. Idempotent
//     once everything is bound.
//
// When --email is not provided we prompt interactively against stdin,
// matching the login flow. Production runs almost always pass --email
// (it's the one piece of user-facing input here and a scriptable seed
// is the homelab norm); the prompt path exists for first-time CLI
// users who don't yet know the flag.
func runUserSeed(ctx context.Context, args []string, stdin io.Reader, out io.Writer, clk clock.Clock) error {
	fs := flag.NewFlagSet("user seed", flag.ContinueOnError)
	fs.SetOutput(out)
	email := fs.String("email", "", "operator email (prompted interactively when omitted)")
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe user seed [--email <email>]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already wrote a message; surface the
		// error so the binary exits non-zero.
		return fmt.Errorf("user seed: %w", err)
	}

	finalEmail := strings.TrimSpace(*email)
	if finalEmail == "" {
		reader := bufio.NewReader(stdin)
		got, err := promptRaw(reader, out, "Operator email", "")
		if err != nil {
			return fmt.Errorf("user seed: prompt email: %w", err)
		}
		finalEmail = strings.TrimSpace(got)
	}
	if finalEmail == "" {
		return errors.New("user seed: email is required")
	}

	db, cleanup, err := openSeedDB(ctx, clk)
	if err != nil {
		return fmt.Errorf("user seed: %w", err)
	}
	defer func() { _ = cleanup() }()

	uid, err := store.SeedOperator(ctx, db, store.SeedOpts{Email: finalEmail})
	if err != nil {
		return fmt.Errorf("user seed: %w", err)
	}
	n, err := store.BackfillOperatorAccounts(ctx, db, uid)
	if err != nil {
		return fmt.Errorf("user seed: backfill: %w", err)
	}
	fprintf(out, "Seeded operator %s (%s); bound %d v1 account row(s).\n",
		string(uid), finalEmail, n)
	return nil
}

// runUserList implements `user list`. It reads directly from the v2
// users table via store.ListUsers (the cross-tenant admin function;
// see internal/store/users.go for the exemption rationale) and prints
// a tab-aligned table.
//
// Refactor target: when M1-10 lands the real Service, swap the
// store.ListUsers call for service.ListUsers(ctx, operatorID, ...) and
// drop the direct DB open. The output shape stays.
func runUserList(ctx context.Context, args []string, out io.Writer, clk clock.Clock) error {
	fs := flag.NewFlagSet("user list", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe user list")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("user list: %w", err)
	}

	db, cleanup, err := openSeedDB(ctx, clk)
	if err != nil {
		return fmt.Errorf("user list: %w", err)
	}
	defer func() { _ = cleanup() }()

	rows, err := store.ListUsers(ctx, db)
	if err != nil {
		return fmt.Errorf("user list: %w", err)
	}
	if len(rows) == 0 {
		fprintln(out, "(no users yet — run `resy-snipe user seed --email <email>` to mint the operator)")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fprintf(tw, "ID\tEMAIL\tROLE\tCREATED\tINVITED_BY\tDISABLED\n")
	for _, r := range rows {
		invitedBy := "-"
		if r.InvitedBy != nil {
			invitedBy = string(*r.InvitedBy)
		}
		disabled := "-"
		if r.DisabledAt != nil {
			disabled = r.DisabledAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		created := "-"
		if !r.CreatedAt.IsZero() {
			created = r.CreatedAt.UTC().Format("2006-01-02T15:04:05Z")
		}
		fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			string(r.ID), r.Email, r.Role, created, invitedBy, disabled)
	}
	if err := tw.Flush(); err != nil {
		return fmt.Errorf("user list: flush: %w", err)
	}
	return nil
}

// runUserInvite implements `user invite <email> [--role=…]`. The
// Service.InviteUser implementation is the Stub until M1-10 lands so
// the call returns ErrNotImplemented — the CLI surfaces this as a
// friendly "coming in M1-10" message with a non-zero exit.
//
// We still validate the arg shape here (email positional, --role
// flag) so that once the real Service is wired the CLI surface
// doesn't change.
func runUserInvite(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("user invite", flag.ContinueOnError)
	fs.SetOutput(out)
	role := fs.String("role", "user", "tenant role (user|admin|readonly)")
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe user invite <email> [--role=user|admin|readonly]")
		fs.PrintDefaults()
	}
	// flag.Parse stops at the first non-flag arg, but the documented
	// CLI surface puts the email first and the --role flag after.
	// Reorder the slice so flags precede positionals before handing
	// it to Parse — this lets the user write either order.
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("user invite: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("user invite: missing <email> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("user invite: unexpected extra arguments: %v", rest[1:])
	}
	email := strings.TrimSpace(rest[0])
	if email == "" {
		return errors.New("user invite: email is required")
	}
	if !validRole(*role) {
		return fmt.Errorf("user invite: invalid --role %q (want user|admin|readonly)", *role)
	}

	// Stub Service today: invocation returns ErrNotImplemented. The
	// caller (operator) is the only authority who can issue invites,
	// so the userID we pass is the placeholder ""; Service.InviteUser
	// will reject anything but a real operator id once M1-10 lands.
	svc := service.Stub{}
	if _, err := svc.InviteUser(ctx, domain.UserID(""), email, *role); err != nil {
		if errors.Is(err, service.ErrNotImplemented) {
			return fmt.Errorf("user invite: %s", notImplementedMsg)
		}
		return fmt.Errorf("user invite: %w", err)
	}
	// Real-Service path (unreachable today): print the invite token.
	return nil
}

// runUserAcceptInvite implements `user accept-invite <token>`. Same
// deferred-implementation treatment as `user invite`.
func runUserAcceptInvite(ctx context.Context, args []string, out io.Writer) error {
	fs := flag.NewFlagSet("user accept-invite", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.Usage = func() {
		fprintln(out, "Usage: resy-snipe user accept-invite <token>")
		fs.PrintDefaults()
	}
	// See runUserInvite for the flags-first / positionals split
	// rationale. accept-invite has no flags today but the same
	// split makes the path uniform for the eventual --label etc.
	flagsFirst, positionals := splitFlagsAndPositionals(args)
	if err := fs.Parse(flagsFirst); err != nil {
		return fmt.Errorf("user accept-invite: %w", err)
	}
	rest := positionals
	rest = append(rest, fs.Args()...)
	if len(rest) == 0 {
		fs.Usage()
		return errors.New("user accept-invite: missing <token> positional argument")
	}
	if len(rest) > 1 {
		fs.Usage()
		return fmt.Errorf("user accept-invite: unexpected extra arguments: %v", rest[1:])
	}
	token := strings.TrimSpace(rest[0])
	if token == "" {
		return errors.New("user accept-invite: token is required")
	}

	// Stub today; the real Service.AcceptInvite owns email + password
	// capture (typically via a follow-up TUI). We pass empty values
	// here because the Stub ignores them; once M1-10 lands the CLI
	// will grow stdin prompts for the new tenant's chosen credentials.
	svc := service.Stub{}
	if _, _, err := svc.AcceptInvite(ctx, token, "", ""); err != nil {
		if errors.Is(err, service.ErrNotImplemented) {
			return fmt.Errorf("user accept-invite: %s", notImplementedMsg)
		}
		return fmt.Errorf("user accept-invite: %w", err)
	}
	return nil
}

// splitFlagsAndPositionals reorders args so flag.FlagSet.Parse sees
// every flag token before any positional arg. Go's flag package stops
// parsing at the first non-flag token, which would force the CLI
// surface to mandate flag-first ordering — surprising for a user who
// types `resy-snipe user invite x@y.io --role=admin` and expects the
// natural order to work.
//
// The split is shallow: a token starting with "-" or "--" is treated
// as a flag (along with the following token when the flag form lacks
// "="). A bare "--" terminator transfers everything after it to the
// positionals slice unmodified. The recognized flags for `invite` /
// `accept-invite` are simple --key=value or boolean — the heuristic
// is correct for both today and stays correct as long as new flags
// keep the same shape.
func splitFlagsAndPositionals(args []string) (flags, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			positionals = append(positionals, args[i+1:]...)
			return flags, positionals
		case strings.HasPrefix(a, "-") && a != "-":
			flags = append(flags, a)
			// If the flag uses "--key value" form (no '=' inside),
			// the value follows in the next slot — pull it along
			// so Parse sees the pair as one unit.
			if !strings.Contains(a, "=") && i+1 < len(args) &&
				!strings.HasPrefix(args[i+1], "-") &&
				!isKnownBoolFlag(strings.TrimLeft(a, "-")) {
				flags = append(flags, args[i+1])
				i++
			}
		default:
			positionals = append(positionals, a)
		}
	}
	return flags, positionals
}

// isKnownBoolFlag lists boolean flags exposed by the user-subcommand
// and quest-subcommand flag sets. The split heuristic in
// splitFlagsAndPositionals consults it to avoid pulling a positional
// arg into the "flag value" slot when the preceding token is a bool
// flag like "-h", "--help", or "-yes".
func isKnownBoolFlag(name string) bool {
	switch name {
	case "h", "help", "yes":
		return true
	}
	return false
}

// validRole mirrors the CHECK constraint on users.role in migration
// 0002_v2_multi_user.sql. Validating in the CLI gives the user an
// immediate error (with a useful hint) instead of waiting for the SQL
// constraint to fail downstream.
func validRole(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case "admin", "user", "readonly":
		return true
	}
	return false
}

// openSeedDB opens + migrates the default sqlite DB and returns the
// raw *sql.DB plus a cleanup. Split out from openCLIClient /
// openSnipeBackend because `user seed` and `user list` need the raw
// handle directly (store.SeedOperator / store.ListUsers take
// *sql.DB) — they don't need the resy.Client or the SQLiteStore
// wrapper. The cleanup closes the DB exactly once.
//
// The clk parameter is unused today but kept on the signature so
// later versions that record a deterministic-time audit row (M1-11)
// can read the clock without a downstream signature change.
func openSeedDB(ctx context.Context, _ clock.Clock) (*sql.DB, func() error, error) {
	db, err := store.Open(ctx, "")
	if err != nil {
		return nil, nil, fmt.Errorf("open db: %w", err)
	}
	if err := store.Migrate(ctx, db); err != nil {
		_ = db.Close()
		return nil, nil, fmt.Errorf("migrate db: %w", err)
	}
	return db, db.Close, nil
}
