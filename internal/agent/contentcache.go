// SPDX-License-Identifier: MIT
// Copyright (c) 2026 Anthony Green <green@moxielogic.com>

package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/atgreen/dirq/internal/config"
)

// contentCache is a content-addressable file cache. Files are stored
// by their SHA256 hash so identical content is stored once regardless
// of how many times it's pushed.
type contentCache struct {
	dir string
	mu  sync.RWMutex
}

func newContentCache() *contentCache {
	dir := filepath.Join(config.DataDir(), "content_cache")
	os.MkdirAll(dir, 0700)
	return &contentCache{dir: dir}
}

// Put stores content by its hash. Returns the hash.
func (c *contentCache) Put(content []byte) string {
	hash := hashContent(content)
	c.mu.Lock()
	defer c.mu.Unlock()
	path := filepath.Join(c.dir, hash)
	// Skip if already cached.
	if _, err := os.Stat(path); err == nil {
		return hash
	}
	os.WriteFile(path, content, 0600)
	return hash
}

// Get retrieves content by hash. Returns nil if not cached.
func (c *contentCache) Get(hash string) ([]byte, error) {
	// Validate hash format to prevent path traversal.
	if len(hash) != 64 {
		return nil, fmt.Errorf("invalid content hash length: %d", len(hash))
	}
	for _, r := range hash {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return nil, fmt.Errorf("invalid content hash character: %c", r)
		}
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	return os.ReadFile(filepath.Join(c.dir, hash))
}

// Has returns true if the hash is in the cache.
func (c *contentCache) Has(hash string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	_, err := os.Stat(filepath.Join(c.dir, hash))
	return err == nil
}

func hashContent(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}
