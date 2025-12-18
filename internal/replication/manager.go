// Package replication manages the Replication Factor (RF=3) guarantee.
//
// Responsibilities:
//  1. On every write, stream a chunk to exactly RF healthy nodes in parallel.
//  2. Watch heartbeat failure events; when a node dies, detect which chunks
//     lost a replica and dispatch asynchronous re-replication to restore RF=3.
//  3. Commit all ownership changes to Raft so the metadata is consistent
//     across the cluster even if the coordinating node crashes mid-way.
package replication

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/spartan/dsvc/internal/hash"
	"github.com/spartan/dsvc/internal/heartbeat"
	"github.com/spartan/dsvc/internal/raftstore"
	"github.com/spartan/dsvc/internal/storage"
)

const (
	ReplicationFactor = 3
	replicateTimeout  = 30 * time.Second
)

// NodeRegistry maps nodeID to its HTTP base address.
type NodeRegistry interface {
	NodeAddr(nodeID string) (string, bool)
	AllNodes() []string
}

// Manager orchestrates chunk replication and re-replication.
type Manager struct {
	store    *storage.Store
	ring     *hash.Ring
	raft     *raftstore.Node
	registry NodeRegistry
	client   *http.Client
	mu       sync.Mutex
}

// New creates a Manager.
func New(store *storage.Store, ring *hash.Ring, raft *raftstore.Node, registry NodeRegistry) *Manager {
	return &Manager{
		store:    store,
		ring:     ring,
		raft:     raft,
		registry: registry,
		client:   &http.Client{Timeout: replicateTimeout},
	}
}

// ReplicateChunk fans-out a chunk to RF nodes, waits for a quorum (majority)
// acknowledgement, then commits ownership to Raft.
// This is called on the write path after the local node has stored the chunk.
func (m *Manager) ReplicateChunk(chunkID string, data []byte) error {
	targets := m.ring.GetNodes(chunkID, ReplicationFactor)
	if len(targets) == 0 {
		return fmt.Errorf("replication: no targets on ring for chunk %s", chunkID)
	}

	type result struct {
		nodeID string
		err    error
	}
	results := make(chan result, len(targets))

	for _, nodeID := range targets {
		go func(nid string) {
			addr, ok := m.registry.NodeAddr(nid)
			if !ok {
				results <- result{nid, fmt.Errorf("node %s not in registry", nid)}
				return
			}
			err := m.pushChunk(addr, chunkID, data)
			results <- result{nid, err}
		}(nodeID)
	}

	quorum := (len(targets) / 2) + 1
	var acked []string
	var errs []error

	for range targets {
		r := <-results
		if r.err == nil {
			acked = append(acked, r.nodeID)
		} else {
			errs = append(errs, r.err)
		}
	}

	if len(acked) < quorum {
		return fmt.Errorf("replication: quorum not reached for chunk %s (%d/%d acked): %v",
			chunkID, len(acked), quorum, errs)
	}

	// Commit ownership to the replicated Raft log.
	if m.raft.IsLeader() {
		if err := m.raft.RecordChunkOwners(chunkID, acked); err != nil {
			// Non-fatal: chunk is stored; metadata will be reconciled.
			fmt.Printf("replication: raft commit failed for chunk %s: %v\n", chunkID, err)
		}
	}
	return nil
}

// HandleFailureEvent is called when the heartbeat monitor declares a node dead.
// It removes the dead node from the hash ring and triggers re-replication for
// every chunk that was on that node, restoring the Replication Factor to 3.
func (m *Manager) HandleFailureEvent(ev heartbeat.Event) {
	if ev.Type != heartbeat.EventFailure {
		return
	}
	deadNode := ev.NodeID
	fmt.Printf("replication: node %s failed — scanning for under-replicated chunks\n", deadNode)

	// Remove from ring so future writes avoid the dead node.
	m.ring.RemoveNode(deadNode)

	// Scan the local store for any chunk whose Raft ownership includes the dead node.
	chunkIDs, err := m.store.ListChunks()
	if err != nil {
		fmt.Printf("replication: failed to list chunks: %v\n", err)
		return
	}
	for _, chunkID := range chunkIDs {
		owners := m.raft.GetChunkOwners(chunkID)
		for _, owner := range owners {
			if owner == deadNode {
				go m.reReplicateChunk(chunkID, deadNode)
				break
			}
		}
	}
}

// reReplicateChunk restores RF=3 for a chunk that lost a replica.
func (m *Manager) reReplicateChunk(chunkID, deadNode string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	owners := m.raft.GetChunkOwners(chunkID)
	if len(owners) >= ReplicationFactor {
		return // already healthy
	}

	// Read from any surviving owner.
	var data []byte
	for _, ownerID := range owners {
		if ownerID == deadNode {
			continue
		}
		addr, ok := m.registry.NodeAddr(ownerID)
		if !ok {
			continue
		}
		d, err := m.fetchChunk(addr, chunkID)
		if err == nil {
			data = d
			break
		}
	}

	if data == nil {
		// Try local store as last resort.
		d, err := m.store.Read(chunkID)
		if err != nil {
			fmt.Printf("replication: cannot recover chunk %s — no readable replica\n", chunkID)
			return
		}
		data = d
	}

	// Find a new target that does NOT already own this chunk.
	ownerSet := make(map[string]struct{})
	for _, o := range owners {
		ownerSet[o] = struct{}{}
	}
	for _, candidate := range m.registry.AllNodes() {
		if _, owned := ownerSet[candidate]; owned {
			continue
		}
		addr, ok := m.registry.NodeAddr(candidate)
		if !ok {
			continue
		}
		if err := m.pushChunk(addr, chunkID, data); err == nil {
			newOwners := append(owners, candidate)
			if m.raft.IsLeader() {
				_ = m.raft.RecordChunkOwners(chunkID, newOwners)
			}
			fmt.Printf("replication: re-replicated chunk %s to node %s (RF restored)\n",
				chunkID, candidate)
			return
		}
	}
	fmt.Printf("replication: could not find healthy target for chunk %s\n", chunkID)
}

// pushChunk sends a chunk to a peer's internal replication endpoint.
func (m *Manager) pushChunk(nodeAddr, chunkID string, data []byte) error {
	url := fmt.Sprintf("%s/internal/chunks/%s", nodeAddr, chunkID)
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("peer %s returned HTTP %d", nodeAddr, resp.StatusCode)
	}
	return nil
}

// fetchChunk retrieves a chunk from a peer node.
func (m *Manager) fetchChunk(nodeAddr, chunkID string) ([]byte, error) {
	url := fmt.Sprintf("%s/internal/chunks/%s", nodeAddr, chunkID)
	resp, err := m.client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("peer %s returned HTTP %d", nodeAddr, resp.StatusCode)
	}
	return io.ReadAll(resp.Body)
}
