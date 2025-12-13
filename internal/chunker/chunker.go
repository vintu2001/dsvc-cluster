// Package chunker splits arbitrary byte streams into fixed-size encrypted blocks.
// Each chunk is identified by a content-addressed SHA-256 hash of its plaintext,
// which also serves as the key on the consistent hash ring.
package chunker

import (
	"crypto/sha256"
	"fmt"
	"io"
)

const DefaultChunkSize = 64 * 1024 * 1024 // 64 MiB — mirrors Amazon S3 multipart minimum

// Chunk represents a single immutable data block.
type Chunk struct {
	ID    string // hex(sha256(Data)) — content-addressable
	Index int    // position in the original file
	Data  []byte // raw bytes (up to ChunkSize)
	Size  int    // actual byte count (last chunk may be smaller)
}

// Chunker splits a reader into fixed-size Chunk objects.
type Chunker struct {
	chunkSize int
}

// New creates a Chunker with a configurable chunk size in bytes.
func New(chunkSize int) *Chunker {
	if chunkSize <= 0 {
		chunkSize = DefaultChunkSize
	}
	return &Chunker{chunkSize: chunkSize}
}

// Split reads from r until EOF and emits Chunks over the returned channel.
// The caller must drain the channel; Split closes it when done.
// Returns an error channel — non-nil error means the split was incomplete.
func (c *Chunker) Split(r io.Reader) (<-chan *Chunk, <-chan error) {
	chunks := make(chan *Chunk, 8)
	errc := make(chan error, 1)

	go func() {
		defer close(chunks)
		defer close(errc)

		buf := make([]byte, c.chunkSize)
		index := 0

		for {
			n, err := io.ReadFull(r, buf)
			if n > 0 {
				data := make([]byte, n)
				copy(data, buf[:n])
				chunks <- &Chunk{
					ID:    contentHash(data),
					Index: index,
					Data:  data,
					Size:  n,
				}
				index++
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				break
			}
			if err != nil {
				errc <- fmt.Errorf("chunker read error at index %d: %w", index, err)
				return
			}
		}
	}()

	return chunks, errc
}

// Reassemble orders a slice of chunks by Index and concatenates their Data.
// Use this on the read path after fetching all chunks from the cluster.
func Reassemble(chunks []*Chunk) []byte {
	// Sort by index in-place
	sorted := make([]*Chunk, len(chunks))
	copy(sorted, chunks)
	for i := 0; i < len(sorted)-1; i++ {
		for j := i + 1; j < len(sorted); j++ {
			if sorted[j].Index < sorted[i].Index {
				sorted[i], sorted[j] = sorted[j], sorted[i]
			}
		}
	}

	var out []byte
	for _, ch := range sorted {
		out = append(out, ch.Data...)
	}
	return out
}

// contentHash returns the hex-encoded SHA-256 of data — used as the chunk's immutable ID.
func contentHash(data []byte) string {
	h := sha256.Sum256(data)
	return fmt.Sprintf("%x", h)
}
