package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spartan/dsvc/internal/api"
	"github.com/spartan/dsvc/internal/cluster"
	"github.com/spartan/dsvc/internal/hash"
	"github.com/spartan/dsvc/internal/heartbeat"
	"github.com/spartan/dsvc/internal/raftstore"
	"github.com/spartan/dsvc/internal/replication"
	"github.com/spartan/dsvc/internal/storage"
)

func main() {
	cfg := loadConfig()
	log.Printf("[dsvc] starting node %s | http=:%s raft=%s bootstrap=%v",
		cfg.nodeID, cfg.httpPort, cfg.raftAddr, cfg.bootstrap)

	store, err := storage.New(cfg.dataDir)
	must("storage", err)

	ring := hash.NewRing(150)
	ring.AddNode(cfg.nodeID)

	raftNode, err := raftstore.NewNode(raftstore.Config{
		NodeID:    cfg.nodeID,
		BindAddr:  cfg.raftAddr,
		DataDir:   filepath.Join(cfg.dataDir, "raft"),
		Bootstrap: cfg.bootstrap,
		Peers:     cfg.peers,
	})
	must("raft", err)

	registry := cluster.New()
	registry.Register(cfg.nodeID, fmt.Sprintf("http://localhost:%s", cfg.httpPort))

	replMgr := replication.New(store, ring, raftNode, registry)

	handler := api.New(cfg.nodeID, store, ring, raftNode, registry, replMgr)
	srv := &http.Server{
		Addr:         ":" + cfg.httpPort,
		Handler:      handler.Router(),
		ReadTimeout:  5 * time.Minute, // large files take time
		WriteTimeout: 5 * time.Minute,
		IdleTimeout:  120 * time.Second,
	}

	hbMonitor := heartbeat.New()
	for _, peer := range cfg.peers {
		// peers is []raftAddr; derive HTTP addr by convention (raftPort+1000=httpPort)
		// In a real cluster, peers announce their HTTP addresses on join.
		hbMonitor.Register(peer, "http://"+peer)
	}
	hbMonitor.Start()

	// Drive failure events into the replication manager.
	go func() {
		for ev := range hbMonitor.Events() {
			switch ev.Type {
			case heartbeat.EventFailure:
				log.Printf("[dsvc] node %s declared DEAD — initiating re-replication", ev.NodeID)
				go replMgr.HandleFailureEvent(ev)
				if raftNode.IsLeader() {
					_ = raftNode.DeregisterNode(ev.NodeID)
				}
			case heartbeat.EventRecovery:
				log.Printf("[dsvc] node %s is ALIVE again", ev.NodeID)
				ring.AddNode(ev.NodeID)
				if raftNode.IsLeader() {
					_ = raftNode.RegisterNode(ev.NodeID)
				}
			}
		}
	}()

	go func() {
		log.Printf("[dsvc] HTTP listening on :%s", cfg.httpPort)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server error: %v", err)
		}
	}()

	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[dsvc] shutting down gracefully …")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	srv.Shutdown(ctx)
	hbMonitor.Stop()
	raftNode.Shutdown()
	log.Println("[dsvc] goodbye")
}

type config struct {
	nodeID    string
	httpPort  string
	raftAddr  string
	dataDir   string
	bootstrap bool
	peers     []string
}

func loadConfig() config {
	nodeID := env("DSVC_NODE_ID", "node-1")
	httpPort := env("DSVC_HTTP_PORT", "8080")
	raftPort := env("DSVC_RAFT_PORT", "7000")
	dataDir := env("DSVC_DATA_DIR", filepath.Join("data", nodeID))
	bootstrap := env("DSVC_BOOTSTRAP", "false") == "true"
	peersRaw := env("DSVC_PEERS", "")

	var peers []string
	if peersRaw != "" {
		for _, p := range strings.Split(peersRaw, ",") {
			if p = strings.TrimSpace(p); p != "" {
				peers = append(peers, p)
			}
		}
	}

	return config{
		nodeID:    nodeID,
		httpPort:  httpPort,
		raftAddr:  fmt.Sprintf("0.0.0.0:%s", raftPort),
		dataDir:   dataDir,
		bootstrap: bootstrap,
		peers:     peers,
	}
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func must(label string, err error) {
	if err != nil {
		log.Fatalf("[dsvc] fatal: %s: %v", label, err)
	}
}
