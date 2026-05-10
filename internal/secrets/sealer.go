package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"resy-snipe/internal/clock"
	"resy-snipe/internal/domain"
)

// NonceLen is the AES-GCM nonce length in bytes (96 bits).
const NonceLen = 12

// adSeparator partitions associated-data components so a row that
// changes one of (user_id, account_id, kind) without re-encryption
// fails the AEAD check. 0x1f (US, "unit separator") doesn't appear in
// any of our id formats, so this is a clean injective encoding.
const adSeparator = byte(0x1f)

// Sealer is the package's only public type. One Sealer per daemon
// process; constructed at boot and Close()d at shutdown. Methods are
// safe for concurrent use (the AEAD cipher is stateless once
// constructed and *sql.DB serializes writes).
type Sealer struct {
	db      *sql.DB
	clock   clock.Clock
	aead    cipher.AEAD
	key     *Key
	version int
}

// NewSealer wires the Sealer with a derived key, the meta version,
// and the *sql.DB. The Sealer takes ownership of key: a subsequent
// Sealer.Close wipes and Munlocks the underlying buffer. Callers
// MUST NOT Close key separately.
func NewSealer(db *sql.DB, key *Key, meta Meta, c clock.Clock) (*Sealer, error) {
	if db == nil {
		return nil, errors.New("secrets: NewSealer: nil db")
	}
	if key == nil || len(key.bytes()) != KeyLen {
		return nil, errors.New("secrets: NewSealer: bad key")
	}
	if c == nil {
		return nil, errors.New("secrets: NewSealer: nil clock")
	}
	block, err := aes.NewCipher(key.bytes())
	if err != nil {
		return nil, fmt.Errorf("secrets: aes.NewCipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: cipher.NewGCM: %w", err)
	}
	if aead.NonceSize() != NonceLen {
		return nil, fmt.Errorf("secrets: unexpected nonce size %d", aead.NonceSize())
	}
	return &Sealer{
		db:      db,
		clock:   c,
		aead:    aead,
		key:     key,
		version: meta.ActiveVersion,
	}, nil
}

// Seal encrypts plaintext under the active key and writes the row,
// replacing any existing (userID, accountID, kind) row. Idempotent in
// the sense that re-Sealing the same plaintext is safe — the new
// ciphertext uses a fresh nonce and Open returns the same bytes.
func (s *Sealer) Seal(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind, plaintext []byte) error {
	if userID == "" || accountID == "" {
		return errors.New("secrets: Seal: userID and accountID required")
	}
	if !kind.Valid() {
		return fmt.Errorf("secrets: Seal: unknown kind %q", string(kind))
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("secrets: Seal: generate nonce: %w", err)
	}
	ad := associatedData(userID, accountID, kind)
	ciphertext := s.aead.Seal(nil, nonce, plaintext, ad)

	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO secrets (user_id, account_id, kind, ciphertext, nonce, version, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (user_id, account_id, kind) DO UPDATE SET
			ciphertext = excluded.ciphertext,
			nonce      = excluded.nonce,
			version    = excluded.version,
			created_at = excluded.created_at`,
		string(userID), string(accountID), string(kind), ciphertext, nonce, s.version, s.clock.Now().UnixMilli(),
	); err != nil {
		return fmt.Errorf("secrets: Seal: %w", err)
	}
	return nil
}

// Open decrypts the row. Returns ErrNotFound if absent; ErrTampered
// if the AEAD verification fails.
func (s *Sealer) Open(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind) ([]byte, error) {
	if userID == "" || accountID == "" {
		return nil, errors.New("secrets: Open: userID and accountID required")
	}
	if !kind.Valid() {
		return nil, fmt.Errorf("secrets: Open: unknown kind %q", string(kind))
	}
	var (
		ciphertext []byte
		nonce      []byte
		version    int
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT ciphertext, nonce, version FROM secrets WHERE user_id = ? AND account_id = ? AND kind = ?`,
		string(userID), string(accountID), string(kind),
	).Scan(&ciphertext, &nonce, &version)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil, ErrNotFound
	case err != nil:
		return nil, fmt.Errorf("secrets: Open: query: %w", err)
	}
	if len(nonce) != NonceLen {
		return nil, fmt.Errorf("secrets: Open: stored nonce length %d, want %d", len(nonce), NonceLen)
	}
	ad := associatedData(userID, accountID, kind)
	plaintext, err := s.aead.Open(nil, nonce, ciphertext, ad)
	if err != nil {
		// AEAD verification failures cannot distinguish "bytes were
		// flipped" from "wrong key" without out-of-band knowledge.
		// Surfacing ErrTampered keeps the public contract clean;
		// daemon boot uses secrets_meta.active_version vs row.version
		// to upgrade the diagnosis to ErrWrongKey for operator messaging.
		return nil, fmt.Errorf("%w: %w", ErrTampered, err)
	}
	return plaintext, nil
}

// List returns the kinds present for (userID, accountID). The result
// is unordered. Used by `accounts inspect` to enumerate what is
// stored without revealing plaintext.
func (s *Sealer) List(ctx context.Context, userID domain.UserID, accountID domain.AccountID) ([]Kind, error) {
	if userID == "" || accountID == "" {
		return nil, errors.New("secrets: List: userID and accountID required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind FROM secrets WHERE user_id = ? AND account_id = ?`,
		string(userID), string(accountID),
	)
	if err != nil {
		return nil, fmt.Errorf("secrets: List: %w", err)
	}
	defer rows.Close()
	var out []Kind
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("secrets: List scan: %w", err)
		}
		out = append(out, Kind(k))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("secrets: List iterate: %w", err)
	}
	return out, nil
}

// Delete removes the (userID, accountID, kind) row. Returns
// ErrNotFound if no row matched. The FK cascade on users / accounts
// handles bulk removal; this method is for explicit per-kind purge
// (e.g. accounts forget-password).
func (s *Sealer) Delete(ctx context.Context, userID domain.UserID, accountID domain.AccountID, kind Kind) error {
	if userID == "" || accountID == "" {
		return errors.New("secrets: Delete: userID and accountID required")
	}
	if !kind.Valid() {
		return fmt.Errorf("secrets: Delete: unknown kind %q", string(kind))
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM secrets WHERE user_id = ? AND account_id = ? AND kind = ?`,
		string(userID), string(accountID), string(kind),
	)
	if err != nil {
		return fmt.Errorf("secrets: Delete: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("secrets: Delete rows affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// Close wipes the in-process key and Munlocks its page. Idempotent.
// Subsequent Seal / Open calls return errors because the AEAD cipher
// holds a reference to the now-zeroed bytes; do not Close until the
// last caller is done.
func (s *Sealer) Close() error {
	if s == nil || s.key == nil {
		return nil
	}
	err := s.key.Close()
	s.key = nil
	return err
}

// associatedData binds a row to its (userID, accountID, kind) triple
// so swapping any of the three on disk fails the AEAD check. The
// separator 0x1f is the ASCII unit-separator and never appears in any
// of our id formats.
func associatedData(userID domain.UserID, accountID domain.AccountID, kind Kind) []byte {
	u := []byte(userID)
	a := []byte(accountID)
	k := []byte(kind)
	out := make([]byte, 0, len(u)+len(a)+len(k)+2)
	out = append(out, u...)
	out = append(out, adSeparator)
	out = append(out, a...)
	out = append(out, adSeparator)
	out = append(out, k...)
	return out
}
