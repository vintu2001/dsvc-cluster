// Package hash implements a Consistent Hashing ring with virtual nodes.
// When a node is added/removed, only the immediate neighbors on the ring
// are affected — minimizing data movement during cluster expansion.
package hash

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
	"sync"
)

const defaultVirtualNodes = 150 // virtual nodes per physical node for uniform distribution

// Ring is a thread-safe consistent hash ring.
type Ring struct {
	mu           sync.RWMutex
	virtualNodes int
	ring         map[uint32]string // hash position → node ID
	positions    []uint32          // sorted ring positions
}

// NewRing constructs an empty Ring with the given number of virtual nodes per physical node.
func NewRing(virtualNodes int) *Ring {
	if virtualNodes <= 0 {
		virtualNodes = defaultVirtualNodes
	}
	return &Ring{
		virtualNodes: virtualNodes,
		ring:         make(map[uint32]string),
	}
}

// AddNode inserts a physical node and its virtual replicas onto the ring.
func (r *Ring) AddNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := 0; i < r.virtualNodes; i++ {
		key := fmt.Sprintf("%s#vnode-%d", nodeID, i)
		pos := hashKey(key)
		r.ring[pos] = nodeID
		r.positions = append(r.positions, pos)
	}
	sort.Slice(r.positions, func(i, j int) bool {
		return r.positions[i] < r.positions[j]
	})
}

// RemoveNode removes a physical node and all its virtual replicas from the ring.
func (r *Ring) RemoveNode(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	for i := 0; i < r.virtualNodes; i++ {
		key := fmt.Sprintf("%s#vnode-%d", nodeID, i)
		pos := hashKey(key)
		delete(r.ring, pos)
	}

	// Rebuild sorted positions slice without removed entries.
	newPositions := r.positions[:0]
	for _, pos := range r.positions {
		if _, ok := r.ring[pos]; ok {
			newPositions = append(newPositions, pos)
		}
	}
	r.positions = newPositions
}

// GetNodes returns the first `count` distinct physical nodes responsible for the given key,
// walking clockwise around the ring. This is the Replication Factor mechanism.
func (r *Ring) GetNodes(key string, count int) []string {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if len(r.positions) == 0 {
		return nil
	}

	pos := hashKey(key)
	idx := sort.Search(len(r.positions), func(i int) bool {
		return r.positions[i] >= pos
	})

	seen := make(map[string]struct{})
	var nodes []string

	for i := 0; i < len(r.positions) && len(nodes) < count; i++ {
		ringIdx := (idx + i) % len(r.positions)
		nodeID := r.ring[r.positions[ringIdx]]
		if _, ok := seen[nodeID]; !ok {
			seen[nodeID] = struct{}{}
			nodes = append(nodes, nodeID)
		}
	}
	return nodes
}

// GetNode returns the single primary node responsible for the given key.
func (r *Ring) GetNode(key string) string {
	nodes := r.GetNodes(key, 1)
	if len(nodes) == 0 {
		return ""
	}
	return nodes[0]
}

// NodeCount returns the number of distinct physical nodes currently on the ring.
func (r *Ring) NodeCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	seen := make(map[string]struct{})
	for _, n := range r.ring {
		seen[n] = struct{}{}
	}
	return len(seen)
}

// hashKey maps a string key to a uint32 ring position using SHA-256.
func hashKey(key string) uint32 {
	sum := sha256.Sum256([]byte(key))
	return binary.BigEndian.Uint32(sum[:4])
}
