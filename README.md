# DSVC — Distributed Virtualized Cloud Storage Engine

A production-grade, fault-tolerant distributed object storage engine built with **Go**, **Kubernetes**, and **Ansible**. Features automatic replication, consistent hashing, Raft consensus, and self-healing cluster management.

[![Go Version](https://img.shields.io/badge/Go-1.22+-00ADD8?style=flat&logo=go)](https://go.dev/)
[![Docker](https://img.shields.io/badge/Docker-Ready-2496ED?style=flat&logo=docker)](https://www.docker.com/)
[![License](https://img.shields.io/badge/License-MIT-green.svg)](LICENSE)

---

## 🚀 Quick Start

### Prerequisites
- **Docker** & Docker Compose
- **Go 1.22+** (for local development)
- **Kubernetes** (optional, for production deployment)
- **Ansible** (optional, for bare-metal provisioning)

### Run with Docker (Recommended)

```bash
# Clone the repository
git clone https://github.com/your-username/dsvc.git
cd dsvc

# Build the Docker image
docker build -t dsvc-node:latest .

# Start a 3-node cluster
docker compose up -d

# Verify all nodes are running
docker ps

# Check cluster status
curl http://localhost:8081/v1/cluster/status | jq
```

### Run Locally (Without Docker)

```bash
# Terminal 1 - Bootstrap leader
DSVC_NODE_ID=node-1 DSVC_HTTP_PORT=8081 DSVC_RAFT_PORT=7001 \
DSVC_BOOTSTRAP=true go run ./cmd/storagenode

# Terminal 2 - Follower node
DSVC_NODE_ID=node-2 DSVC_HTTP_PORT=8082 DSVC_RAFT_PORT=7002 \
DSVC_PEERS=localhost:7001 go run ./cmd/storagenode

# Terminal 3 - Follower node
DSVC_NODE_ID=node-3 DSVC_HTTP_PORT=8083 DSVC_RAFT_PORT=7003 \
DSVC_PEERS=localhost:7001 go run ./cmd/storagenode
```

---

## 📖 Usage

### Upload a File
```bash
echo "Hello, distributed world!" > test.txt
curl -X PUT http://localhost:8081/v1/objects/my-file \
  --data-binary @test.txt
```

**Response:**
```json
{
  "key": "my-file",
  "chunks": 1,
  "ids": ["8d5ae765..."]
}
```

### Download a File
```bash
curl http://localhost:8081/v1/objects/my-file
# Output: Hello, distributed world!
```

### Check Cluster Status
```bash
curl http://localhost:8081/v1/cluster/status | jq
```

**Response:**
```json
{
  "is_leader": true,
  "leader": "172.18.0.2:7000",
  "live_nodes": ["node-1", "node-2", "node-3"],
  "ring_nodes": 3
}
```

### Delete a File
```bash
curl -X DELETE http://localhost:8081/v1/objects/my-file
```

---

## 🏗️ Architecture

```
                    ┌─────────────────────────────────────────────────────┐
  Client PUT /v1/objects/big-file.tar                                     │
        │                                                                  │
        ▼                                                                  │
  ┌─────────────┐    ┌──────────────┐    ┌──────────────┐                 │
  │  HTTP API   │───▶│   Chunker    │───▶│ Consistent   │                 │
  │  (Gorilla)  │    │ (64MB blocks)│    │ Hashing Ring │                 │
  └─────────────┘    └──────────────┘    └──────┬───────┘                 │
                                                 │ GetNodes(chunkID, RF=3) │
                                    ┌────────────▼──────────────┐         │
                                    │   Replication Manager     │         │
                                    │  Fan-out to 3 nodes in    │         │
                                    │  parallel; wait for quorum│         │
                                    └──────────┬────────────────┘         │
                                               │                          │
               ┌───────────────────────────────┼──────────────────┐       │
               ▼                               ▼                  ▼       │
         ┌──────────┐                   ┌──────────┐        ┌──────────┐  │
         │  node-0  │                   │  node-1  │        │  node-2  │  │
         │ (Leader) │◀─── Raft Log ─────│(Follower)│        │(Follower)│  │
         │ /data/   │                   │ /data/   │        │ /data/   │  │
         └──────────┘                   └──────────┘        └──────────┘  │
               │                                                           │
               └── Commit "200 OK" only after quorum ack ─────────────────┘
```

---

## Project Structure

```
DSVC N1/
├── cmd/
│   └── storagenode/
│       └── main.go              # Node binary entrypoint
├── internal/
│   ├── api/
│   │   └── handler.go           # HTTP handlers (public + internal endpoints)
│   ├── chunker/
│   │   └── chunker.go           # File splitter (64 MiB chunks, content-addressed)
│   ├── cluster/
│   │   └── registry.go          # Thread-safe node address registry
│   ├── hash/
│   │   └── consistent.go        # Consistent hashing ring (150 virtual nodes/physical)
│   ├── heartbeat/
│   │   └── monitor.go           # Peer liveness monitor (ping /healthz every 2s)
│   ├── raftstore/
│   │   ├── raft.go              # Raft consensus node (hashicorp/raft wrapper)
│   │   └── bolt_store.go        # BoltDB storage backend note
│   ├── replication/
│   │   └── manager.go           # RF=3 fan-out + automatic re-replication on failure
│   └── storage/
│       └── storage.go           # Flat-file chunk store (atomic writes)
├── k8s/
│   ├── namespace.yaml           # dsvc namespace
│   ├── service.yaml             # Headless + ClusterIP services
│   ├── statefulset.yaml         # 3-replica StatefulSet with PVCs
│   └── hpa.yaml                 # HorizontalPodAutoscaler (3–9 replicas)
├── ansible/
│   ├── inventory.yml            # Node inventory (3 nodes by default)
│   ├── provision.yml            # Bootstrap playbook (Docker, sysctl, systemd)
│   ├── expand_cluster.yml       # Add a new node to a running cluster
│   └── templates/
│       └── dsvc-node.service.j2 # Systemd service template
├── Dockerfile                   # Multi-stage build (scratch final image)
├── docker-compose.yml           # 3-node local dev cluster
├── go.mod
└── README.md
```

---

## Core Concepts Implemented

### 1. Consistent Hashing (`internal/hash/consistent.go`)
- **150 virtual nodes** per physical node for uniform distribution
- `GetNodes(key, RF)` walks the ring clockwise, returning the first `RF` **distinct** physical nodes
- `RemoveNode` redraws only the affected arc — all other data stays put
- Impact of adding/removing a node: `O(1/N)` data movement (not a full reshuffle)

### 2. Raft Consensus (`internal/raftstore/raft.go`)
- Uses `hashicorp/raft` with BoltDB-backed log + stable stores
- Leader applies `CmdSetChunkOwners` log entries; all followers replicate
- On Leader failure: election completes in **≤500ms** (configurable)
- FSM snapshot compaction prevents unbounded log growth

### 3. RF=3 Replication (`internal/replication/manager.go`)
- Every chunk PUT fans out to 3 nodes in **parallel goroutines**
- Returns `200 OK` only after `⌊RF/2⌋ + 1` nodes acknowledge (quorum = 2 of 3)
- On node failure: scans for under-replicated chunks, fetches from a surviving replica, pushes to a new host

### 4. Failure Detection (`internal/heartbeat/monitor.go`)
- Pings `/healthz` on every peer every **2 seconds**
- 3 consecutive misses → `EventFailure` emitted → replication manager re-replicates
- Node revival → `EventRecovery` → node re-added to hash ring

### 5. File Chunking (`internal/chunker/chunker.go`)
- Streams any `io.Reader` into **64 MiB blocks**
- Each chunk ID = `sha256(data)` — content-addressed (deduplication built-in)
- `Reassemble()` sorts by index and concatenates on reads

---

## 🛠️ Development

### Build
```bash
go build -o dsvc-node ./cmd/storagenode
```

### Build Docker Image
```bash
docker build -t dsvc-node:latest .
```

---

## ☸️ Kubernetes Deployment

```bash
# Create namespace and deploy
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/service.yaml
kubectl apply -f k8s/statefulset.yaml
kubectl apply -f k8s/hpa.yaml

# Watch pods start
kubectl -n dsvc get pods -w

# Access the cluster
kubectl -n dsvc port-forward svc/dsvc-api 8080:80
curl http://localhost:8080/v1/cluster/status
```

---

## 🔧 Ansible Provisioning

### Provision Nodes
```bash
# Edit ansible/inventory.yml with your node IPs
ansible-playbook -i ansible/inventory.yml ansible/provision.yml
```

### Expand Cluster
```bash
# Add node-4 to inventory.yml, then:
ansible-playbook -i ansible/inventory.yml ansible/expand_cluster.yml --limit node-4
```

---

## 📊 API Reference

| Method | Endpoint | Description |
|--------|----------|-------------|
| `PUT` | `/v1/objects/{key}` | Upload a file |
| `GET` | `/v1/objects/{key}` | Download a file |
| `DELETE` | `/v1/objects/{key}` | Delete a file |
| `GET` | `/v1/cluster/status` | Cluster topology |
| `GET` | `/healthz` | Health check |

---

## ⚙️ Configuration

| Variable | Default | Description |
|----------|---------|-------------|
| `DSVC_NODE_ID` | `node-1` | Unique node identifier |
| `DSVC_HTTP_PORT` | `8080` | HTTP API port |
| `DSVC_RAFT_PORT` | `7000` | Raft consensus port |
| `DSVC_DATA_DIR` | `data/{node-id}` | Data directory |
| `DSVC_BOOTSTRAP` | `false` | Bootstrap cluster (first node only) |
| `DSVC_PEERS` | _(empty)_ | Comma-separated peer Raft addresses |

---

## 🎯 Features

- ✅ **Automatic Replication** - RF=3 with quorum writes
- ✅ **Consistent Hashing** - Minimal data movement on scale-out
- ✅ **Raft Consensus** - Leader election in ≤500ms
- ✅ **Self-Healing** - Automatic re-replication on node failure
- ✅ **Content-Addressed Storage** - Deduplication built-in
- ✅ **Horizontal Scaling** - Add nodes without downtime
- ✅ **Kubernetes Native** - StatefulSets with persistent volumes

---

## 📁 Project Structure

```
DSVC/
├── cmd/storagenode/          # Main binary
├── internal/
│   ├── api/                  # HTTP handlers
│   ├── chunker/              # File chunking (64MB blocks)
│   ├── cluster/              # Node registry
│   ├── hash/                 # Consistent hashing
│   ├── heartbeat/            # Failure detection
│   ├── raftstore/            # Raft consensus
│   ├── replication/          # RF=3 manager
│   └── storage/              # Disk persistence
├── k8s/                      # Kubernetes manifests
├── ansible/                  # IaC playbooks
├── Dockerfile
├── docker-compose.yml
└── README.md
```

---

## 🔬 Testing the System

### Test Failure Recovery
```bash
# Kill a node
docker stop dsvc-node-2

# Check cluster detects failure (after 3 missed heartbeats)
docker logs dsvc-node-1 | grep "declared DEAD"

# Upload a file - cluster still works
curl -X PUT http://localhost:8081/v1/objects/test --data "Testing failover"

# Restart the node
docker start dsvc-node-2

# Verify recovery
docker logs dsvc-node-1 | grep "ALIVE again"
```

### Test Large Files
```bash
# Generate 200MB file (will split into 4 chunks @ 64MB each)
dd if=/dev/urandom of=large.bin bs=1M count=200

# Upload
time curl -X PUT http://localhost:8081/v1/objects/bigfile \
  --data-binary @large.bin

# Download and verify
curl http://localhost:8081/v1/objects/bigfile -o downloaded.bin
diff large.bin downloaded.bin && echo "✓ Files match!"
```

---

## 🐛 Troubleshooting

### Cluster Won't Start
```bash
# Check logs
docker logs dsvc-node-1

# Verify network
docker network inspect dsvcn1_dsvc-net

# Reset cluster
docker compose down -v
docker compose up -d
```

### Node Fails Health Check
```bash
# Test manually
docker exec dsvc-node-1 wget -qO- http://localhost:8080/healthz
```

---

## 📝 License

MIT License - see [LICENSE](LICENSE) file for details.

---

## 🤝 Contributing

Pull requests welcome! Please ensure:
- Code is formatted: `go fmt ./...`
- Commits are descriptive

---

## 🙏 Acknowledgments

Built with:
- [hashicorp/raft](https://github.com/hashicorp/raft) - Consensus protocol
- [gorilla/mux](https://github.com/gorilla/mux) - HTTP routing
- [bbolt](https://github.com/etcd-io/bbolt) - Embedded key-value store
