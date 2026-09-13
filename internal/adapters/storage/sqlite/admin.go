package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"cdamp/internal/domain"
)

// GetAdminCredential returns the singleton admin credential, or
// domain.ErrNotFound if none has been bootstrapped yet.
func (s *Store) GetAdminCredential(ctx context.Context) (*domain.AdminCredential, error) {
	row := s.db.QueryRowContext(ctx, `SELECT token_hash, created_at FROM admin_credential WHERE id = 1`)

	var (
		tokenHash string
		createdAt int64
	)
	if err := row.Scan(&tokenHash, &createdAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrNotFound
		}
		return nil, fmt.Errorf("getting admin credential: %w", err)
	}
	return &domain.AdminCredential{
		TokenHash: tokenHash,
		CreatedAt: time.Unix(createdAt, 0).UTC(),
	}, nil
}

// SaveAdminCredential persists c as the singleton admin credential.
func (s *Store) SaveAdminCredential(ctx context.Context, c *domain.AdminCredential) error {
	_, err := s.db.ExecContext(ctx,
		"INSERT INTO admin_credential (id, token_hash, created_at) VALUES (1, ?, ?)",
		c.TokenHash, c.CreatedAt.Unix(),
	)
	if err != nil {
		return fmt.Errorf("saving admin credential: %w", err)
	}
	return nil
}
