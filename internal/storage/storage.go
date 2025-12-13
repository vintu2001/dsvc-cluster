// Package storage manages on-disk persistence of individual chunks.
// Each chunk is stored as a flat file at: <dataDir>/<chunkID>.chunk
// This mirrors how object storage backends work at the block level.
package storage

import (
	"fmt"
	"os"
	"path/filepath"
)

// Store is a flat-file chunk store backed by a local directory.
type Store struct {
	dataDir string
}

// New opens (or creates) a Store at the given directory.
func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("storage: cannot create data dir %q: %w", dataDir, err)
	}
	return &Store{dataDir: dataDir}, nil
}

// Write persists a chunk's bytes to disk.
// Idempotent: writing the same chunkID twice is a no-op (content-addressed).
func (s *Store) Write(chunkID string, data []byte) error {
	path := s.chunkPath(chunkID)
	if _, err := os.Stat(path); err == nil {
		return nil // chunk already exists — content is immutable
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("storage: write temp file for chunk %q: %w", chunkID, err)
	}
	// Atomic rename guarantees no partial reads.
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("storage: atomic rename for chunk %q: %w", chunkID, err)
	}
	return nil
}

// Read retrieves the raw bytes for a chunk from disk.
func (s *Store) Read(chunkID string) ([]byte, error) {
	data, err := os.ReadFile(s.chunkPath(chunkID))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("storage: chunk %q not found", chunkID)
		}
		return nil, fmt.Errorf("storage: read chunk %q: %w", chunkID, err)
	}
	return data, nil
}

// Delete removes a chunk from disk. Used when re-balancing after a node joins.
func (s *Store) Delete(chunkID string) error {
	err := os.Remove(s.chunkPath(chunkID))
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("storage: delete chunk %q: %w", chunkID, err)
	}
	return nil
}

// Has reports whether the chunk is locally available.
func (s *Store) Has(chunkID string) bool {
	_, err := os.Stat(s.chunkPath(chunkID))
	return err == nil
}

// ListChunks returns all chunk IDs stored locally.
func (s *Store) ListChunks() ([]string, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, fmt.Errorf("storage: list chunks: %w", err)
	}
	var ids []string
	for _, e := range entries {
		if !e.IsDir() && filepath.Ext(e.Name()) == ".chunk" {
			id := e.Name()[:len(e.Name())-6] // strip ".chunk"
			ids = append(ids, id)
		}
	}
	return ids, nil
}

func (s *Store) chunkPath(chunkID string) string {
	return filepath.Join(s.dataDir, chunkID+".chunk")
}
