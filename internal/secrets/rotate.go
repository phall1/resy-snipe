package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	"resy-snipe/internal/domain"
)

// CheckMixedVersion verifies that every row in `secrets` has a
// version matching `secrets_meta.active_version`. Mid-rotation
// crashes inside the BEGIN/COMMIT can't produce a mismatch — SQLite
// rolls the whole transaction back — so a mismatch always means
// either a botched outside-the-txn rotate (impossible by design but
// the check is cheap) or operator-supplied tampering with sqlite3.
//
// Returns ErrMixedVersion wrapping the stale row identifiers so the
// operator's runbook can name the rows in the boot-refusal log.
func CheckMixedVersion(ctx context.Context, db *sql.DB) error {
	if db == nil {
		return errors.New("secrets: CheckMixedVersion: nil db")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.user_id, s.account_id, s.kind, s.version
		FROM secrets s, secrets_meta m
		WHERE m.id = 1 AND s.version != m.active_version`)
	if err != nil {
		return fmt.Errorf("secrets: CheckMixedVersion: %w", err)
	}
	defer rows.Close()

	var stale []string
	for rows.Next() {
		var userID, accountID, kind string
		var ver int
		if err := rows.Scan(&userID, &accountID, &kind, &ver); err != nil {
			return fmt.Errorf("secrets: CheckMixedVersion scan: %w", err)
		}
		stale = append(stale, fmt.Sprintf("(%s,%s,%s)@v%d", userID, accountID, kind, ver))
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("secrets: CheckMixedVersion iterate: %w", err)
	}
	if len(stale) > 0 {
		return fmt.Errorf("%w: %s", ErrMixedVersion, strings.Join(stale, ", "))
	}
	return nil
}

// rotateMu serializes Rotate against concurrent Seal/Open on the same
// Sealer. Held for the duration of the transaction so callers don't
// see a half-rotated state.
//
// rotateMu is a package-level sync.Mutex because rotate is a daemon-
// global operation (the design says so — there is at most one
// in-flight rotate per daemon, and it blocks new Seal calls until the
// COMMIT lands).
var rotateMu sync.Mutex

// Rotate re-encrypts every row in the `secrets` table under newKey
// inside a single SQL transaction, bumps `secrets_meta.active_version`,
// and swaps the Sealer's in-process AEAD/key over to newKey. The old
// key is wiped as the last step.
//
// Either every row is on newKey and active_version is bumped, or
// nothing changed (SQLite rolls the transaction back on any error).
// A daemon killed before COMMIT comes back up on the old key with all
// rows still openable — that's the point of the atomicity.
//
// The newKey is consumed: a successful Rotate calls Close on the old
// key and adopts the new key for future Seal/Open calls. A failed
// Rotate leaves the Sealer on the old key and Closes the new key so
// callers don't leak a derived buffer.
func (s *Sealer) Rotate(ctx context.Context, newKey *Key) error {
	if s == nil {
		return errors.New("secrets: Rotate: nil sealer")
	}
	if newKey == nil || len(newKey.bytes()) != KeyLen {
		return errors.New("secrets: Rotate: bad new key")
	}

	rotateMu.Lock()
	defer rotateMu.Unlock()

	newBlock, err := aes.NewCipher(newKey.bytes())
	if err != nil {
		_ = newKey.Close()
		return fmt.Errorf("secrets: Rotate aes.NewCipher: %w", err)
	}
	newAEAD, err := cipher.NewGCM(newBlock)
	if err != nil {
		_ = newKey.Close()
		return fmt.Errorf("secrets: Rotate cipher.NewGCM: %w", err)
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelDefault})
	if err != nil {
		_ = newKey.Close()
		return fmt.Errorf("secrets: Rotate begin: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()

	type row struct {
		userID, accountID, kind string
		ct, nonce               []byte
	}
	// Lock the table for the duration of the rotate. The PRAGMA
	// busy_timeout in the open path covers contention from any other
	// in-flight readers; once we've successfully selected the rows
	// here we hold a SQLite write lock until COMMIT.
	batch, err := func() ([]row, error) {
		rows, err := tx.QueryContext(ctx,
			`SELECT user_id, account_id, kind, ciphertext, nonce FROM secrets`)
		if err != nil {
			return nil, fmt.Errorf("secrets: Rotate select: %w", err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.userID, &r.accountID, &r.kind, &r.ct, &r.nonce); err != nil {
				return nil, fmt.Errorf("secrets: Rotate scan: %w", err)
			}
			out = append(out, r)
		}
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("secrets: Rotate iterate: %w", err)
		}
		return out, nil
	}()
	if err != nil {
		_ = newKey.Close()
		return err
	}

	newVersion := s.version + 1

	for _, r := range batch {
		ad := associatedData(domain.UserID(r.userID), domain.AccountID(r.accountID), Kind(r.kind))
		plaintext, err := s.aead.Open(nil, r.nonce, r.ct, ad)
		if err != nil {
			_ = newKey.Close()
			return fmt.Errorf("secrets: Rotate open %s/%s/%s: %w", r.userID, r.accountID, r.kind, err)
		}
		nonce := make([]byte, NonceLen)
		if _, err := rand.Read(nonce); err != nil {
			_ = newKey.Close()
			return fmt.Errorf("secrets: Rotate generate nonce: %w", err)
		}
		ct := newAEAD.Seal(nil, nonce, plaintext, ad)
		if _, err := tx.ExecContext(ctx, `
			UPDATE secrets SET ciphertext = ?, nonce = ?, version = ?
			WHERE user_id = ? AND account_id = ? AND kind = ?`,
			ct, nonce, newVersion, r.userID, r.accountID, r.kind,
		); err != nil {
			_ = newKey.Close()
			return fmt.Errorf("secrets: Rotate update: %w", err)
		}
		// Wipe the in-memory plaintext as soon as the row lands.
		for i := range plaintext {
			plaintext[i] = 0
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE secrets_meta SET active_version = ? WHERE id = 1`, newVersion,
	); err != nil {
		_ = newKey.Close()
		return fmt.Errorf("secrets: Rotate bump version: %w", err)
	}

	if err := tx.Commit(); err != nil {
		_ = newKey.Close()
		return fmt.Errorf("secrets: Rotate commit: %w", err)
	}
	committed = true

	// Hot-swap the in-process state. From here on every Seal/Open
	// uses the new AEAD; the old key is wiped before we release the
	// rotate mutex.
	old := s.key
	s.aead = newAEAD
	s.key = newKey
	s.version = newVersion
	_ = old.Close()
	return nil
}
