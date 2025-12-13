// Package cluster maintains the node registry: a thread-safe map of nodeID → HTTP address.
// Nodes register themselves on startup by calling the leader's /internal/nodes endpoint.
// The registry is the authoritative source used by the replication manager.
package cluster

import (
	"sync"
)

// Registry is a thread-safe map of nodeID → base HTTP address.
type Registry struct {
	mu    sync.RWMutex
	nodes map[string]string
}

// New creates an empty Registry.
func New() *Registry {
	return &Registry{nodes: make(map[string]string)}
}

// Register adds or updates a node's HTTP address.
func (r *Registry) Register(nodeID, addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.nodes[nodeID] = addr
}

// Remove deletes a node from the registry.
func (r *Registry) Remove(nodeID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.nodes, nodeID)
}

// NodeAddr returns the HTTP address of nodeID, and whether it was found.
func (r *Registry) NodeAddr(nodeID string) (string, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	addr, ok := r.nodes[nodeID]
	return addr, ok
}

// AllNodes returns all currently registered node IDs.
func (r *Registry) AllNodes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.nodes))
	for id := range r.nodes {
		out = append(out, id)
	}
	return out
}
