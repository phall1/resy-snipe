package store

import "context"

// SeedUserForTest inserts a minimal users row. External
// internal/store_test.go files use this to satisfy the
// sessions.user_id FK without promoting a full users API into Phase 1.
// The file's _test.go suffix means the symbol is compiled only at
// test time; production builds can never reach it.
func SeedUserForTest(s *SQLiteStore, id string) error {
	_, err := s.db.ExecContext(context.Background(),
		`INSERT INTO users (id) VALUES (?)`, id)
	return err
}
