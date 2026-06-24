// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/atgreen/dirq/internal/db"
	"golang.org/x/crypto/bcrypt"
)

const (
	tokenByteLength  = 32
	tokenPrefixChars = 8
)

// CreateToken generates a random API token, stores its bcrypt hash and a
// non-secret prefix for fast lookup, and returns the plaintext token.
func (d *DB) CreateToken(ctx context.Context, name, scope string, aapUsers []string) (string, error) {
	raw := make([]byte, tokenByteLength)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	plaintext := hex.EncodeToString(raw)

	hash, err := bcrypt.GenerateFromPassword([]byte(plaintext), bcrypt.DefaultCost)
	if err != nil {
		return "", fmt.Errorf("hash token: %w", err)
	}

	prefix := plaintext[:tokenPrefixChars]
	id := generateUUID()

	_, err = d.db.ExecContext(ctx, `
		INSERT INTO api_tokens (id, name, token_prefix, token_hash, scope, aap_users)
		VALUES (?, ?, ?, ?, ?, ?)`,
		id, name, prefix, string(hash), scope, db.EncodeAAPUsers(aapUsers),
	)
	if err != nil {
		return "", err
	}

	return plaintext, nil
}

// ValidateToken checks a plaintext token against stored hashes.
func (d *DB) ValidateToken(ctx context.Context, plaintext string) (db.Token, error) {
	if len(plaintext) < tokenPrefixChars {
		return db.Token{}, sql.ErrNoRows
	}
	prefix := plaintext[:tokenPrefixChars]

	rows, err := d.db.QueryContext(ctx, `
		SELECT id, name, token_hash, scope, aap_users, created_at, last_used
		FROM api_tokens
		WHERE token_prefix = ?`, prefix)
	if err != nil {
		return db.Token{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var t db.Token
		var hash string
		var aapUsers string
		var createdAt string
		var lastUsed sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &hash, &t.Scope, &aapUsers, &createdAt, &lastUsed); err != nil {
			return db.Token{}, err
		}
		t.AAPUsers = db.ParseAAPUsers(aapUsers)
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if lastUsed.Valid {
			lu, _ := time.Parse(time.RFC3339, lastUsed.String)
			t.LastUsed = &lu
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil {
			now := time.Now().UTC().Format(time.RFC3339)
			_, _ = d.db.ExecContext(ctx, `
				UPDATE api_tokens SET last_used = ? WHERE id = ?`, now, t.ID)
			return t, nil
		}
	}
	if err := rows.Err(); err != nil {
		return db.Token{}, err
	}

	return db.Token{}, sql.ErrNoRows
}

// ListTokens returns all API tokens (without hashes).
func (d *DB) ListTokens(ctx context.Context) ([]db.Token, error) {
	rows, err := d.db.QueryContext(ctx, `
		SELECT id, name, scope, aap_users, created_at, last_used
		FROM api_tokens
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []db.Token
	for rows.Next() {
		var t db.Token
		var aapUsers string
		var createdAt string
		var lastUsed sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &aapUsers, &createdAt, &lastUsed); err != nil {
			return nil, err
		}
		t.AAPUsers = db.ParseAAPUsers(aapUsers)
		t.CreatedAt, _ = time.Parse(time.RFC3339, createdAt)
		if lastUsed.Valid {
			lu, _ := time.Parse(time.RFC3339, lastUsed.String)
			t.LastUsed = &lu
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// DeleteToken removes an API token by name.
func (d *DB) DeleteToken(ctx context.Context, name string) error {
	result, err := d.db.ExecContext(ctx, `DELETE FROM api_tokens WHERE name = ?`, name)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
