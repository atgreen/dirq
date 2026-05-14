// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"

	"github.com/jackc/pgx/v5"
	"golang.org/x/crypto/bcrypt"
)

const (
	tokenByteLength  = 32
	tokenPrefixChars = 8 // hex chars stored for indexed lookup
)

// CreateToken generates a random API token, stores its bcrypt hash and a
// non-secret prefix for fast lookup, and returns the plaintext token.
func (db *DB) CreateToken(ctx context.Context, name, scope string) (string, error) {
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

	_, err = db.pool.Exec(ctx, `
		INSERT INTO api_tokens (name, token_prefix, token_hash, scope)
		VALUES ($1, $2, $3, $4)`,
		name, prefix, string(hash), scope,
	)
	if err != nil {
		return "", err
	}

	return plaintext, nil
}

// ValidateToken checks a plaintext token against stored hashes.
// Uses the token prefix for an indexed lookup, then verifies with bcrypt.
// On success it updates last_used and returns the matching Token.
func (db *DB) ValidateToken(ctx context.Context, plaintext string) (Token, error) {
	if len(plaintext) < tokenPrefixChars {
		return Token{}, pgx.ErrNoRows
	}
	prefix := plaintext[:tokenPrefixChars]

	rows, err := db.pool.Query(ctx, `
		SELECT id, name, token_hash, scope, created_at, last_used
		FROM api_tokens
		WHERE token_prefix = $1`, prefix)
	if err != nil {
		return Token{}, err
	}
	defer rows.Close()

	for rows.Next() {
		var t Token
		var hash string
		if err := rows.Scan(&t.ID, &t.Name, &hash, &t.Scope, &t.CreatedAt, &t.LastUsed); err != nil {
			return Token{}, err
		}
		if bcrypt.CompareHashAndPassword([]byte(hash), []byte(plaintext)) == nil {
			_, _ = db.pool.Exec(ctx, `
				UPDATE api_tokens SET last_used = now() WHERE id = $1`, t.ID)
			return t, nil
		}
	}
	if err := rows.Err(); err != nil {
		return Token{}, err
	}

	return Token{}, pgx.ErrNoRows
}

// ListTokens returns all API tokens (without hashes).
func (db *DB) ListTokens(ctx context.Context) ([]Token, error) {
	rows, err := db.pool.Query(ctx, `
		SELECT id, name, scope, created_at, last_used
		FROM api_tokens
		ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var tokens []Token
	for rows.Next() {
		var t Token
		if err := rows.Scan(&t.ID, &t.Name, &t.Scope, &t.CreatedAt, &t.LastUsed); err != nil {
			return nil, err
		}
		tokens = append(tokens, t)
	}
	return tokens, rows.Err()
}

// DeleteToken removes an API token by name.
func (db *DB) DeleteToken(ctx context.Context, name string) error {
	tag, err := db.pool.Exec(ctx, `DELETE FROM api_tokens WHERE name = $1`, name)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
