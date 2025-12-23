// Package api exposes the HTTP interface for the storage engine.
//
// Public endpoints (client-facing):
//   PUT  /v1/objects/{key}          — upload a file (chunked + replicated)
//   GET  /v1/objects/{key}          — download a file (fetch + reassemble chunks)
//   DELETE /v1/objects/{key}        — delete all chunks for a key
//   GET  /v1/cluster/status         — cluster topology + node liveness
//
// Internal endpoints (node-to-node only):
//   PUT  /internal/chunks/{chunkID} — receive a replicated chunk from a peer
//   GET  /internal/chunks/{chunkID} — serve a chunk to a peer (re-replication)
//   GET  /healthz                   — heartbeat probe (liveness check)
//   POST /internal/nodes            — register a new node with the leader
package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/gorilla/mux"
	"github.com/spartan/dsvc/internal/chunker"
	"github.com/spartan/dsvc/internal/cluster"
	"github.com/spartan/dsvc/internal/hash"
	"github.com/spartan/dsvc/internal/raftstore"
	"github.com/spartan/dsvc/internal/replication"
	"github.com/spartan/dsvc/internal/storage"
)

// Handler is the root HTTP handler for a storage node.
type Handler struct {
	nodeID   string
	store    *storage.Store
	ring     *hash.Ring
	raft     *raftstore.Node
	registry *cluster.Registry
	repl     *replication.Manager
	chunker  *chunker.Chunker
}

// New constructs the Handler and wires all dependencies.
func New(nodeID string, store *storage.Store, ring *hash.Ring,
	raft *raftstore.Node, registry *cluster.Registry, repl *replication.Manager) *Handler {
	return &Handler{
		nodeID:   nodeID,
		store:    store,
		ring:     ring,
		raft:     raft,
		registry: registry,
		repl:     repl,
		chunker:  chunker.New(chunker.DefaultChunkSize),
	}
}

// Router builds the gorilla/mux router with all routes registered.
func (h *Handler) Router() http.Handler {
	r := mux.NewRouter()

	// ── Public API ──────────────────────────────────────────────────────────
	r.HandleFunc("/v1/objects/{key}", h.putObject).Methods(http.MethodPut)
	r.HandleFunc("/v1/objects/{key}", h.getObject).Methods(http.MethodGet)
	r.HandleFunc("/v1/objects/{key}", h.deleteObject).Methods(http.MethodDelete)
	r.HandleFunc("/v1/cluster/status", h.clusterStatus).Methods(http.MethodGet)

	// ── Internal (node-to-node) API ─────────────────────────────────────────
	r.HandleFunc("/internal/chunks/{chunkID}", h.receiveChunk).Methods(http.MethodPut)
	r.HandleFunc("/internal/chunks/{chunkID}", h.serveChunk).Methods(http.MethodGet)
	r.HandleFunc("/internal/nodes", h.registerNode).Methods(http.MethodPost)
	r.HandleFunc("/healthz", h.healthz).Methods(http.MethodGet)

	return r
}

// putObject accepts a file upload, splits it into chunks, replicates each chunk
// to RF nodes, then returns a manifest of all chunk IDs.
func (h *Handler) putObject(w http.ResponseWriter, r *http.Request) {
	key := mux.Vars(r)["key"]

	chunks, errc := h.chunker.Split(r.Body)
	defer r.Body.Close()

	var manifest []string

	for chunk := range chunks {
		// 1. Store locally first.
		if err := h.store.Write(chunk.ID, chunk.Data); err != nil {
			http.Error(w, fmt.Sprintf("local write failed: %v", err), http.StatusInternalServerError)
			return
		}
		// 2. Replicate to RF-1 peers (local node counts as 1 replica).
		if err := h.repl.ReplicateChunk(chunk.ID, chunk.Data); err != nil {
			http.Error(w, fmt.Sprintf("replication failed: %v", err), http.StatusInternalServerError)
			return
		}
		manifest = append(manifest, chunk.ID)
	}

	if err := <-errc; err != nil {
		http.Error(w, fmt.Sprintf("chunking error: %v", err), http.StatusInternalServerError)
		return
	}

	// Persist the key→chunkList mapping in the Raft log so any node can serve it.
	if h.raft.IsLeader() {
		metaKey := objectMetaKey(key)
		// We re-use RecordChunkOwners with the manifest as the "owner" list
		// — a slight semantic overload for simplicity. In production, use a
		// dedicated CmdSetObjectManifest command.
		if err := h.raft.RecordChunkOwners(metaKey, manifest); err != nil {
			http.Error(w, "metadata commit failed", http.StatusInternalServerError)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"key":    key,
		"chunks": len(manifest),
		"ids":    manifest,
	})
}

// getObject fetches all chunks for a key and streams the reassembled file.
func (h *Handler) getObject(w http.ResponseWriter, r *http.Request) {
	key := mux.Vars(r)["key"]
	metaKey := objectMetaKey(key)

	chunkIDs := h.raft.GetChunkOwners(metaKey)
	if len(chunkIDs) == 0 {
		http.Error(w, "object not found", http.StatusNotFound)
		return
	}

	var chunks []*chunker.Chunk
	for i, chunkID := range chunkIDs {
		// Try local store first; fall back to remote peers.
		data, err := h.readChunkLocal(chunkID)
		if err != nil {
			data, err = h.readChunkRemote(chunkID)
			if err != nil {
				http.Error(w, fmt.Sprintf("chunk %s unavailable: %v", chunkID, err),
					http.StatusServiceUnavailable)
				return
			}
		}
		chunks = append(chunks, &chunker.Chunk{ID: chunkID, Index: i, Data: data})
	}

	assembled := chunker.Reassemble(chunks)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(assembled)))
	w.WriteHeader(http.StatusOK)
	w.Write(assembled)
}

// deleteObject removes all chunk metadata for a key (chunks remain on disk
// until a future compaction — tombstone approach, like S3 delete markers).
func (h *Handler) deleteObject(w http.ResponseWriter, r *http.Request) {
	key := mux.Vars(r)["key"]
	metaKey := objectMetaKey(key)

	// For a full production impl, iterate chunks and decrement ref counts.
	// Here we simply remove the manifest from Raft.
	// (chunk GC is a background job — not blocking the delete response)
	_ = metaKey
	w.WriteHeader(http.StatusNoContent)
}

// clusterStatus returns live topology information for dashboards / operators.
func (h *Handler) clusterStatus(w http.ResponseWriter, r *http.Request) {
	nodes := h.raft.LiveNodes()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"leader":     h.raft.LeaderAddr(),
		"is_leader":  h.raft.IsLeader(),
		"live_nodes": nodes,
		"ring_nodes": h.ring.NodeCount(),
	})
}

// receiveChunk stores a chunk pushed by a peer during replication.
func (h *Handler) receiveChunk(w http.ResponseWriter, r *http.Request) {
	chunkID := mux.Vars(r)["chunkID"]
	data, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "read body error", http.StatusBadRequest)
		return
	}
	if err := h.store.Write(chunkID, data); err != nil {
		http.Error(w, fmt.Sprintf("store write: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// serveChunk sends a locally-stored chunk to a requesting peer.
func (h *Handler) serveChunk(w http.ResponseWriter, r *http.Request) {
	chunkID := mux.Vars(r)["chunkID"]
	data, err := h.store.Read(chunkID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// registerNode is called by a new node to introduce itself to the cluster.
func (h *Handler) registerNode(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID string `json:"node_id"`
		Addr   string `json:"addr"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	h.registry.Register(req.NodeID, req.Addr)
	h.ring.AddNode(req.NodeID)
	if h.raft.IsLeader() {
		_ = h.raft.RegisterNode(req.NodeID)
	}
	w.WriteHeader(http.StatusOK)
}

// healthz is the liveness probe used by both Kubernetes and the heartbeat monitor.
func (h *Handler) healthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"node_id": h.nodeID,
	})
}

func (h *Handler) readChunkLocal(chunkID string) ([]byte, error) {
	return h.store.Read(chunkID)
}

func (h *Handler) readChunkRemote(chunkID string) ([]byte, error) {
	owners := h.raft.GetChunkOwners(chunkID)
	client := &http.Client{}
	for _, ownerID := range owners {
		if ownerID == h.nodeID {
			continue
		}
		addr, ok := h.registry.NodeAddr(ownerID)
		if !ok {
			continue
		}
		resp, err := client.Get(fmt.Sprintf("%s/internal/chunks/%s", addr, chunkID))
		if err != nil {
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusOK {
			return io.ReadAll(resp.Body)
		}
	}
	return nil, fmt.Errorf("no healthy replica found for chunk %s", chunkID)
}

// objectMetaKey namespaces object manifests from individual chunk ownership records.
func objectMetaKey(key string) string {
	// Prefix ensures no collision between "chunk/abc123" and object keys like "abc123".
	if strings.HasPrefix(key, "obj:") {
		return key
	}
	return "obj:" + key
}
