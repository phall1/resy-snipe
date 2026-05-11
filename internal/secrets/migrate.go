package secrets

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"

	"resy-snipe/internal/domain"
)

// MigrationReport summarizes the outcome of a MigratePlaintextSessions
// run. Migrated is the count of plaintext-JWT rows that were sealed and
// zeroed; Skipped is the count of rows that needed no action (already
// sealed, empty JWT, or no matching account); Errors collects per-row
// failures that did not abort the transaction (none today — any per-
// row failure rolls the whole batch back, but the field is preserved
// so the surface is stable for future best-effort variants).
type MigrationReport struct {
	Migrated int
	Skipped  int
	Errors   []error
}

// MigratePlaintextSessions walks `sessions` rows whose `jwt` column is
// non-empty, encrypts the JWT with sealer, INSERTs a corresponding row
// into `secrets` keyed by (user_id, account_id, KindResySessionJWT),
// and clears `sessions.jwt` to the empty string so the boot bridge in
// SQLiteStore still recognizes "an account is logged in" without
// holding plaintext.
//
// All work runs inside a single transaction. Either every eligible
// row is sealed and zeroed, or the DB is unchanged. The function is
// idempotent: a session row whose secrets-table peer already exists
// is skipped, as is a row with an empty `jwt` column.
//
// Sessions whose account_id has no matching `accounts` row, or whose
// account has a NULL user_id (M1-16 has not yet bound the v1 import
// to an operator), are logged at WARN and skipped — they don't have a
// homelab tenant to attribute the secret to.
//
// If dryRun is true the function does all the work in a transaction
// that is rolled back at the end; the returned report still reflects
// what would have happened. Use this from `migrate-secrets --dry-run`
// to preview a production migration safely.
func MigratePlaintextSessions(
	ctx context.Context,
	db *sql.DB,
	sealer *Sealer,
	log *slog.Logger,
	dryRun bool,
) (MigrationReport, error) {
	if db == nil {
		return MigrationReport{}, errors.New("secrets: MigratePlaintextSessions: nil db")
	}
	if sealer == nil {
		return MigrationReport{}, errors.New("secrets: MigratePlaintextSessions: nil sealer")
	}
	if log == nil {
		log = slog.Default()
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: begin: %w", err)
	}
	// Default rollback: any non-nil err path or dry-run leaves the DB
	// untouched. The success-and-commit path flips committed=true so
	// the deferred rollback is a no-op.
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	// Read all sessions rows that need migrating. We join to accounts
	// inside the SELECT so a single round-trip carries the user_id
	// (the secrets PK piece that doesn't live on sessions). Rows
	// whose account has NULL user_id are returned with NULL in u_id
	// and handled explicitly below — the JOIN is LEFT so the WARN log
	// names the dangling account rather than silently dropping it.
	rows, err := tx.QueryContext(ctx, `
		SELECT s.account_id, s.jwt, a.user_id
		FROM sessions s
		LEFT JOIN accounts a ON a.id = s.account_id
		WHERE s.jwt != ''`)
	if err != nil {
		return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: query sessions: %w", err)
	}
	defer func() { _ = rows.Close() }()

	type sessionRow struct {
		accountID string
		jwt       string
		userID    sql.NullString
	}
	var batch []sessionRow
	for rows.Next() {
		var r sessionRow
		if err := rows.Scan(&r.accountID, &r.jwt, &r.userID); err != nil {
			return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: scan: %w", err)
		}
		batch = append(batch, r)
	}
	if err := rows.Err(); err != nil {
		return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: iterate: %w", err)
	}

	report := MigrationReport{}
	for _, r := range batch {
		if !r.userID.Valid || r.userID.String == "" {
			log.Warn("secrets.migrate: skipping session with unbound account",
				slog.String("event", "secrets.migrate_unbound_account"),
				slog.String("account_id", r.accountID))
			report.Skipped++
			continue
		}
		userID := domain.UserID(r.userID.String)
		accountID := domain.AccountID(r.accountID)

		// Idempotency check: if a (user, account, kind) row already
		// exists in `secrets`, we leave it alone. This makes re-runs
		// of the migration a no-op once the first run has landed.
		var exists int
		if err := tx.QueryRowContext(ctx, `
			SELECT 1 FROM secrets
			WHERE user_id = ? AND account_id = ? AND kind = ?`,
			string(userID), string(accountID), string(KindResySessionJWT),
		).Scan(&exists); err != nil {
			if !errors.Is(err, sql.ErrNoRows) {
				return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: check existing: %w", err)
			}
		} else {
			// Row already sealed; just make sure the plaintext is
			// cleared. The UPDATE is conditional on jwt != '' so
			// re-runs are still idempotent at the SQL level.
			if _, err := tx.ExecContext(ctx, `
				UPDATE sessions SET jwt = '' WHERE account_id = ? AND jwt != ''`,
				string(accountID),
			); err != nil {
				return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: zero existing plaintext: %w", err)
			}
			report.Skipped++
			continue
		}

		// Defense-in-depth: confirm the user_id we're about to write
		// into `secrets` actually exists in `users`. SQLite FK
		// enforcement is per-connection and database/sql's pool can
		// hand us a connection where PRAGMA foreign_keys is OFF,
		// which would silently let a dangling reference land. An
		// explicit check makes the dangling-reference path produce a
		// loud error regardless of pragma state.
		var userExists int
		err := tx.QueryRowContext(ctx, `SELECT 1 FROM users WHERE id = ?`, string(userID)).Scan(&userExists)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: account %s references missing user %s", accountID, userID)
			}
			return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: verify user: %w", err)
		}

		// Seal the plaintext directly via the AEAD primitive: we need
		// the INSERT to land inside the migration's transaction, and
		// the public Sealer.Seal binds itself to s.db. The encryption
		// step is identical to what Seal does — fresh nonce, the same
		// (user_id, account_id, kind) associated data, the same
		// active-version stamp.
		nonce := make([]byte, NonceLen)
		if _, err := rand.Read(nonce); err != nil {
			return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: nonce: %w", err)
		}
		ad := associatedData(userID, accountID, KindResySessionJWT)
		ciphertext := sealer.aead.Seal(nil, nonce, []byte(r.jwt), ad)

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO secrets (user_id, account_id, kind, ciphertext, nonce, version, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			string(userID), string(accountID), string(KindResySessionJWT),
			ciphertext, nonce, sealer.version, sealer.clock.Now().UnixMilli(),
		); err != nil {
			return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: insert secret: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE sessions SET jwt = '' WHERE account_id = ?`,
			string(accountID),
		); err != nil {
			return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: clear plaintext: %w", err)
		}
		report.Migrated++
	}

	if dryRun {
		// Roll back so production state is untouched. The report
		// already reflects what would have happened.
		return report, nil
	}

	if err := tx.Commit(); err != nil {
		return MigrationReport{}, fmt.Errorf("secrets: MigratePlaintextSessions: commit: %w", err)
	}
	committed = true
	return report, nil
}
