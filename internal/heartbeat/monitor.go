// Package heartbeat monitors peer nodes via periodic pings.
// The Monitor maintains a liveness table, emits FailureEvents when a node
// stops responding, and emits RecoveryEvents when it comes back online.
// These events drive the replication manager to re-replicate orphaned chunks.
package heartbeat

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	defaultInterval = 2 * time.Second
	defaultTimeout  = 1 * time.Second
	defaultMaxMiss  = 3 // consecutive missed pings → node declared dead
)

// EventType distinguishes failure from recovery notifications.
type EventType int

const (
	EventFailure  EventType = iota // node stopped responding
	EventRecovery                  // previously dead node is alive again
)

// Event carries the node ID and type of state change.
type Event struct {
	NodeID string
	Type   EventType
}

// Monitor pings a set of peer nodes and publishes liveness events.
type Monitor struct {
	mu       sync.RWMutex
	nodes    map[string]*peerState // nodeID → state
	events   chan Event
	interval time.Duration
	timeout  time.Duration
	maxMiss  int
	quit     chan struct{}
	client   *http.Client
}

type peerState struct {
	addr   string // HTTP base URL, e.g. "http://node-2:8080"
	misses int    // consecutive missed heartbeats
	alive  bool
}

// New creates a Monitor with default tuning values.
func New() *Monitor {
	return &Monitor{
		nodes:    make(map[string]*peerState),
		events:   make(chan Event, 64),
		interval: defaultInterval,
		timeout:  defaultTimeout,
		maxMiss:  defaultMaxMiss,
		quit:     make(chan struct{}),
		client:   &http.Client{Timeout: defaultTimeout},
	}
}

// Register adds a peer to the monitoring set.
func (m *Monitor) Register(nodeID, httpAddr string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nodes[nodeID] = &peerState{addr: httpAddr, alive: true}
}

// Deregister removes a peer from monitoring.
func (m *Monitor) Deregister(nodeID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.nodes, nodeID)
}

// Events returns the channel on which liveness events are published.
func (m *Monitor) Events() <-chan Event {
	return m.events
}

// Start launches the background ping goroutine.
func (m *Monitor) Start() {
	go func() {
		ticker := time.NewTicker(m.interval)
		defer ticker.Stop()
		for {
			select {
			case <-m.quit:
				return
			case <-ticker.C:
				m.pingAll()
			}
		}
	}()
}

// Stop shuts down the monitor.
func (m *Monitor) Stop() {
	close(m.quit)
}

func (m *Monitor) pingAll() {
	m.mu.Lock()
	snapshot := make(map[string]*peerState, len(m.nodes))
	for id, s := range m.nodes {
		snapshot[id] = s
	}
	m.mu.Unlock()

	for nodeID, state := range snapshot {
		go m.ping(nodeID, state)
	}
}

func (m *Monitor) ping(nodeID string, state *peerState) {
	url := fmt.Sprintf("%s/healthz", state.addr)
	resp, err := m.client.Get(url)
	if err == nil {
		resp.Body.Close()
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	s, ok := m.nodes[nodeID]
	if !ok {
		return
	}

	if err == nil && resp.StatusCode == http.StatusOK {
		if !s.alive {
			s.alive = true
			s.misses = 0
			m.events <- Event{NodeID: nodeID, Type: EventRecovery}
		} else {
			s.misses = 0
		}
	} else {
		s.misses++
		if s.alive && s.misses >= m.maxMiss {
			s.alive = false
			m.events <- Event{NodeID: nodeID, Type: EventFailure}
		}
	}
}

// IsAlive reports whether the given node is currently considered alive.
func (m *Monitor) IsAlive(nodeID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.nodes[nodeID]
	return ok && s.alive
}
