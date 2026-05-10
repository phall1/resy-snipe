package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"resy-snipe/internal/domain"
)

// idempotency.go: persistence-side helpers backing Service.CreateQuest
// and Service.CancelQuest's idempotency contract. The Service depends
// on these via the consumer-side StoreBackend seam in internal/service;
// the package-level shape (vs. a Store interface method) keeps the
// tenant_check_test harness uncluttered and mirrors the quests CRUD
// already filed under package-level functions.
//
// Spec: docs/v2/design/service-layer.md §idempotency. Retention is
// 24h; the Service supplies the TTL and "now" so this layer never
// reads the wall clock on its own (Law 7).
//
// Hashing strategy:
//   * `key_hash` = sha256(userID + ":" + scope + ":" + plaintextKey
//     + ":" + payloadHash). It is the primary-key dedupe identity:
//     the same key with the same payload always lands on the same
//     row.
//   * `plain_hash` = sha256(userID + ":" + scope + ":" + plaintextKey).
//     It is the conflict-detection identity: the same key with a
//     DIFFERENT payload hits a row whose key_hash differs but whose
//     plain_hash matches — the Service interprets that as a reused
//     key with mismatched input and surfaces ErrIdempotencyConflict.
//
// Both hashes are scoped by userID so two tenants picking the same
// plaintext key never collide (multi-user isolation per ADR-0005).

// IdempotencyResultCode is the persisted outcome marker. Mirrors the
// CHECK constraint on idempotency_keys.result_code.
type IdempotencyResultCode string

const (
	IdempotencyOK    IdempotencyResultCode = "ok"
	IdempotencyError IdempotencyResultCode = "error"
)

// HashIdempotencyKey returns the primary-key hash for an idempotency
// row. The four components are joined with ':' separators so a
// payload prefix can never masquerade as the key suffix; sha256
// collapses the result to 32 bytes.
//
// payloadHash is the caller-supplied payload digest (typically the
// goal-hash for create_quest or an empty string for cancel_quest).
// An empty payloadHash is legal and means "no payload component" —
// the conflict-detection path falls back to plain_hash only.
func HashIdempotencyKey(userID domain.UserID, scope, plaintextKey, payloadHash string) []byte {
	sum := sha256.Sum256([]byte(string(userID) + ":" + scope + ":" + plaintextKey + ":" + payloadHash))
	return sum[:]
}

// HashIdempotencyPlain returns the (userID, scope, plaintextKey)
// digest — the secondary lookup identity used to detect key-reuse
// with mismatched input.
func HashIdempotencyPlain(userID domain.UserID, scope, plaintextKey string) []byte {
	sum := sha256.Sum256([]byte(string(userID) + ":" + scope + ":" + plaintextKey))
	return sum[:]
}

// PutIdempotencyResult upserts the outcome row for (userID, scope,
// plaintextKey, payloadHash). errVal nil means "ok"; non-nil persists
// the error's string form so a replay can return the same wrapped
// sentinel.
//
// ttl is added to `now` to compute expires_at; the Service owns both
// per Law 7. targetID is the questID created/canceled — empty on
// failed creates that never minted an ID.
//
// Upsert semantics: a repeat call with the same primary key
// overwrites the prior row. In practice the Service writes each row
// at most once per call, but the ON CONFLICT clause makes the helper
// safe against retried Service code paths that re-enter the persist
// step after a partial failure.
func PutIdempotencyResult(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	targetID string,
	errVal error,
	ttl time.Duration,
	now time.Time,
) error {
	if db == nil {
		return errors.New("PutIdempotencyResult: nil db")
	}
	if userID == "" || scope == "" || plaintextKey == "" {
		return errors.New("PutIdempotencyResult: userID, scope, plaintextKey are required")
	}

	keyHash := HashIdempotencyKey(userID, scope, plaintextKey, payloadHash)
	plainHash := HashIdempotencyPlain(userID, scope, plaintextKey)
	code := IdempotencyOK
	var errStr any
	if errVal != nil {
		code = IdempotencyError
		errStr = errVal.Error()
	}
	var targetArg any
	if targetID != "" {
		targetArg = targetID
	}
	createdAt := now.UTC().Format("2006-01-02T15:04:05.000Z")
	expiresAt := now.Add(ttl).UTC().Format("2006-01-02T15:04:05.000Z")

	_, err := db.ExecContext(ctx, `
		INSERT INTO idempotency_keys
			(key_hash, user_id, scope, plain_hash, target_id, result_code, result_err, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_hash) DO UPDATE SET
			target_id   = excluded.target_id,
			result_code = excluded.result_code,
			result_err  = excluded.result_err,
			created_at  = excluded.created_at,
			expires_at  = excluded.expires_at`,
		keyHash, string(userID), scope, plainHash, targetArg, string(code), errStr, createdAt, expiresAt,
	)
	if err != nil {
		return fmt.Errorf("PutIdempotencyResult: %w", err)
	}
	return nil
}

// IdempotencyLookup is the outcome of GetIdempotencyResult. Found is
// false when no row matches (userID, scope, plaintextKey) at all.
// PayloadMatch is false when a row exists for the key but its
// payloadHash differs — the Service uses that to surface
// ErrIdempotencyConflict. Expired is true when the matching row has
// already passed expires_at; callers treat expired==true as
// not-found.
type IdempotencyLookup struct {
	Found        bool
	PayloadMatch bool
	Expired      bool
	TargetID     string
	ResultCode   IdempotencyResultCode
	ResultErr    string
}

// GetIdempotencyResult looks up the row for (userID, scope,
// plaintextKey, payloadHash).
//
// The lookup proceeds in two stages:
//  1. Match the primary key (userID + scope + plaintextKey +
//     payloadHash). A hit means a prior call with the SAME payload
//     stored a result — return PayloadMatch=true.
//  2. Fall back to (userID, scope, plaintextKey) only. A hit means
//     the key was previously used with a DIFFERENT payload — return
//     Found=true, PayloadMatch=false, and the Service surfaces
//     ErrIdempotencyConflict.
//
// `now` decides expiry — past-expiry rows produce Expired=true so
// the Service treats them as not-found and proceeds fresh.
func GetIdempotencyResult(
	ctx context.Context,
	db *sql.DB,
	userID domain.UserID,
	scope, plaintextKey, payloadHash string,
	now time.Time,
) (IdempotencyLookup, error) {
	if db == nil {
		return IdempotencyLookup{}, errors.New("GetIdempotencyResult: nil db")
	}
	if userID == "" || scope == "" || plaintextKey == "" {
		return IdempotencyLookup{}, errors.New("GetIdempotencyResult: userID, scope, plaintextKey are required")
	}

	keyHash := HashIdempotencyKey(userID, scope, plaintextKey, payloadHash)

	// 1) Exact-payload lookup.
	row := db.QueryRowContext(ctx, `
		SELECT target_id, result_code, result_err, expires_at
		FROM idempotency_keys
		WHERE key_hash = ? AND user_id = ?`,
		keyHash, string(userID),
	)
	lookup, found, err := scanIdempotencyRow(row, now)
	if err != nil {
		return IdempotencyLookup{}, err
	}
	if found {
		lookup.Found = true
		lookup.PayloadMatch = true
		return lookup, nil
	}

	// 2) Same key, mismatched payload? Look up by (userID, scope,
	// plain_hash) and pick the freshest non-expired row.
	plainHash := HashIdempotencyPlain(userID, scope, plaintextKey)
	row = db.QueryRowContext(ctx, `
		SELECT target_id, result_code, result_err, expires_at
		FROM idempotency_keys
		WHERE user_id = ? AND scope = ? AND plain_hash = ?
		ORDER BY created_at DESC
		LIMIT 1`,
		string(userID), scope, plainHash,
	)
	lookup, found, err = scanIdempotencyRow(row, now)
	if err != nil {
		return IdempotencyLookup{}, err
	}
	if found {
		lookup.Found = true
		lookup.PayloadMatch = false
	}
	return lookup, nil
}

// scanIdempotencyRow decodes one row into an IdempotencyLookup,
// returning found=false on sql.ErrNoRows.
func scanIdempotencyRow(row *sql.Row, now time.Time) (IdempotencyLookup, bool, error) {
	var (
		targetID   sql.NullString
		resultCode string
		resultErr  sql.NullString
		expiresAt  string
	)
	if err := row.Scan(&targetID, &resultCode, &resultErr, &expiresAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return IdempotencyLookup{}, false, nil
		}
		return IdempotencyLookup{}, false, fmt.Errorf("scan idempotency: %w", err)
	}
	exp, err := time.Parse("2006-01-02T15:04:05.000Z", expiresAt)
	if err != nil {
		// Fallback: try RFC3339 in case SQLite emitted a slightly
		// different shape (defensive — the schema default uses the
		// strict shape above).
		exp, err = time.Parse(time.RFC3339Nano, expiresAt)
		if err != nil {
			return IdempotencyLookup{}, false, fmt.Errorf("parse expires_at %q: %w", expiresAt, err)
		}
	}
	out := IdempotencyLookup{
		TargetID:   targetID.String,
		ResultCode: IdempotencyResultCode(resultCode),
		ResultErr:  resultErr.String,
	}
	if !now.UTC().Before(exp.UTC()) {
		out.Expired = true
	}
	return out, true, nil
}

// PruneExpiredIdempotency deletes every row whose expires_at is at
// or before `now`. Returns the number of rows removed. Out of M1
// scope (the daemon's periodic-sweep loop will call it); the helper
// is filed here so a future sweep needs no further store-side work.
func PruneExpiredIdempotency(ctx context.Context, db *sql.DB, now time.Time) (int64, error) {
	if db == nil {
		return 0, errors.New("PruneExpiredIdempotency: nil db")
	}
	cutoff := now.UTC().Format("2006-01-02T15:04:05.000Z")
	res, err := db.ExecContext(ctx, `DELETE FROM idempotency_keys WHERE expires_at <= ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("PruneExpiredIdempotency: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("PruneExpiredIdempotency rows: %w", err)
	}
	return n, nil
}
