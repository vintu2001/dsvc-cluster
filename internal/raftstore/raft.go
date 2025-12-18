// Package raftstore wraps hashicorp/raft to provide distributed consensus for
// the cluster's metadata: which chunks live on which nodes, and the current
// leader for each partition.
//
// Architecture:
//   - Each storage node runs a Raft peer.
//   - The Leader applies Log entries that mutate chunk→node mappings.
//   - Followers replicate the log and maintain an identical in-memory state.
//   - On Leader crash, Raft holds a new election in ~500 ms (default timeout).
package raftstore

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/raft"
	raftboltdb "github.com/hashicorp/raft-boltdb/v2"
)

// CommandType identifies the mutation carried in a Raft log entry.
type CommandType uint8

const (
	CmdSetChunkOwners  CommandType = iota // record which nodes own a chunk
	CmdRemoveChunk                        // evict a chunk record (after re-replication)
	CmdRegisterNode                       // new node joined the cluster
	CmdDeregisterNode                     // node left / failed
)

// Command is the payload serialized into each Raft log entry.
type Command struct {
	Type    CommandType `json:"type"`
	ChunkID string      `json:"chunk_id,omitempty"`
	NodeIDs []string    `json:"node_ids,omitempty"`
	NodeID  string      `json:"node_id,omitempty"`
}

// ClusterState is the in-memory FSM state replicated across all peers.
type ClusterState struct {
	mu          sync.RWMutex
	ChunkOwners map[string][]string // chunkID → []nodeID
	LiveNodes   map[string]struct{} // nodeID → present
}

func newClusterState() *ClusterState {
	return &ClusterState{
		ChunkOwners: make(map[string][]string),
		LiveNodes:   make(map[string]struct{}),
	}
}

// FSM implements raft.FSM — the state machine applied to log entries.
type FSM struct {
	state *ClusterState
}

// Apply is called by the Raft library on the Leader AND all Followers
// once a log entry is committed (quorum acknowledged).
func (f *FSM) Apply(l *raft.Log) interface{} {
	var cmd Command
	if err := json.Unmarshal(l.Data, &cmd); err != nil {
		return fmt.Errorf("raft FSM: unmarshal command: %w", err)
	}

	f.state.mu.Lock()
	defer f.state.mu.Unlock()

	switch cmd.Type {
	case CmdSetChunkOwners:
		f.state.ChunkOwners[cmd.ChunkID] = cmd.NodeIDs
	case CmdRemoveChunk:
		delete(f.state.ChunkOwners, cmd.ChunkID)
	case CmdRegisterNode:
		f.state.LiveNodes[cmd.NodeID] = struct{}{}
	case CmdDeregisterNode:
		delete(f.state.LiveNodes, cmd.NodeID)
	}
	return nil
}

// Snapshot captures current state for log compaction.
func (f *FSM) Snapshot() (raft.FSMSnapshot, error) {
	f.state.mu.RLock()
	defer f.state.mu.RUnlock()

	data, err := json.Marshal(f.state)
	if err != nil {
		return nil, err
	}
	return &fsmSnapshot{data: data}, nil
}

// Restore replaces FSM state from a snapshot (used on node restart / new follower).
func (f *FSM) Restore(rc io.ReadCloser) error {
	defer rc.Close()
	var state ClusterState
	if err := json.NewDecoder(rc).Decode(&state); err != nil {
		return err
	}
	f.state.mu.Lock()
	f.state.ChunkOwners = state.ChunkOwners
	f.state.LiveNodes = state.LiveNodes
	f.state.mu.Unlock()
	return nil
}

type fsmSnapshot struct{ data []byte }

func (s *fsmSnapshot) Persist(sink raft.SnapshotSink) error {
	_, err := sink.Write(s.data)
	if err != nil {
		_ = sink.Cancel()
	}
	return err
}
func (s *fsmSnapshot) Release() {}

// Node wraps a raft.Raft instance and exposes cluster-level operations.
type Node struct {
	raft  *raft.Raft
	fsm   *FSM
	state *ClusterState
}

// Config holds all parameters needed to bootstrap a Raft peer.
type Config struct {
	NodeID    string
	BindAddr  string // e.g. "0.0.0.0:7000"
	DataDir   string
	Bootstrap bool     // true only for the very first node in a fresh cluster
	Peers     []string // raft addresses of existing cluster members (empty for bootstrap)
}

// NewNode creates and starts a Raft peer according to cfg.
func NewNode(cfg Config) (*Node, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o755); err != nil {
		return nil, err
	}

	state := newClusterState()
	fsm := &FSM{state: state}

	rc := raft.DefaultConfig()
	rc.LocalID = raft.ServerID(cfg.NodeID)
	rc.HeartbeatTimeout = 500 * time.Millisecond
	rc.ElectionTimeout = 500 * time.Millisecond
	rc.CommitTimeout = 50 * time.Millisecond

	boltDB, err := raftboltdb.NewBoltStore(filepath.Join(cfg.DataDir, "raft.db"))
	if err != nil {
		return nil, fmt.Errorf("raft bolt store: %w", err)
	}
	logStore := boltDB
	stableStore := boltDB
	snapshotStore, err := raft.NewFileSnapshotStore(cfg.DataDir, 3, os.Stderr)
	if err != nil {
		return nil, fmt.Errorf("raft snapshot store: %w", err)
	}

	// Bind to the configured address (e.g., "0.0.0.0:7000" in Docker)
	bindAddr := cfg.BindAddr
	// Advertise address: use BindAddr unless it's 0.0.0.0, then use container hostname
	advertiseAddr := cfg.BindAddr
	if strings.Contains(bindAddr, "0.0.0.0") {
		hostname, _ := os.Hostname()
		port := bindAddr[strings.LastIndex(bindAddr, ":")+1:]
		advertiseAddr = fmt.Sprintf("%s:%s", hostname, port)
	}

	addr, err := net.ResolveTCPAddr("tcp", advertiseAddr)
	if err != nil {
		return nil, err
	}
	transport, err := raft.NewTCPTransport(bindAddr, addr, 3, 10*time.Second, os.Stderr)
	if err != nil {
		return nil, err
	}

	r, err := raft.NewRaft(rc, fsm, logStore, stableStore, snapshotStore, transport)
	if err != nil {
		return nil, err
	}

	if cfg.Bootstrap {
		configuration := raft.Configuration{
			Servers: []raft.Server{
				{ID: raft.ServerID(cfg.NodeID), Address: transport.LocalAddr()},
			},
		}
		r.BootstrapCluster(configuration)
	} else {
		for _, peer := range cfg.Peers {
			future := r.AddVoter(raft.ServerID(peer), raft.ServerAddress(peer), 0, 5*time.Second)
			if err := future.Error(); err != nil {
				// Non-fatal: peer may already be in the cluster.
				fmt.Fprintf(os.Stderr, "raft: add voter %s: %v\n", peer, err)
			}
		}
	}

	return &Node{raft: r, fsm: fsm, state: state}, nil
}

// IsLeader reports whether this node is the current Raft leader.
func (n *Node) IsLeader() bool {
	return n.raft.State() == raft.Leader
}

// LeaderAddr returns the Raft address of the current cluster leader.
func (n *Node) LeaderAddr() string {
	addr, _ := n.raft.LeaderWithID()
	return string(addr)
}

// RecordChunkOwners commits a CmdSetChunkOwners entry to the Raft log.
// Must be called on the Leader.
func (n *Node) RecordChunkOwners(chunkID string, nodeIDs []string) error {
	return n.apply(Command{Type: CmdSetChunkOwners, ChunkID: chunkID, NodeIDs: nodeIDs})
}

// RegisterNode commits a CmdRegisterNode entry.
func (n *Node) RegisterNode(nodeID string) error {
	return n.apply(Command{Type: CmdRegisterNode, NodeID: nodeID})
}

// DeregisterNode commits a CmdDeregisterNode entry.
func (n *Node) DeregisterNode(nodeID string) error {
	return n.apply(Command{Type: CmdDeregisterNode, NodeID: nodeID})
}

// GetChunkOwners reads chunk ownership from the replicated state (reads are local — eventual).
func (n *Node) GetChunkOwners(chunkID string) []string {
	n.state.mu.RLock()
	defer n.state.mu.RUnlock()
	owners := n.state.ChunkOwners[chunkID]
	cp := make([]string, len(owners))
	copy(cp, owners)
	return cp
}

// LiveNodes returns the set of currently registered live nodes.
func (n *Node) LiveNodes() []string {
	n.state.mu.RLock()
	defer n.state.mu.RUnlock()
	nodes := make([]string, 0, len(n.state.LiveNodes))
	for id := range n.state.LiveNodes {
		nodes = append(nodes, id)
	}
	return nodes
}

func (n *Node) apply(cmd Command) error {
	data, err := json.Marshal(cmd)
	if err != nil {
		return err
	}
	f := n.raft.Apply(data, 5*time.Second)
	return f.Error()
}

// Shutdown gracefully stops the Raft peer.
func (n *Node) Shutdown() error {
	return n.raft.Shutdown().Error()
}
