#!/bin/bash
set -e

echo "🚀 DSVC Quick Start"
echo "===================="
echo ""

# Check Docker
if ! command -v docker &> /dev/null; then
    echo "❌ Docker not found. Please install Docker first."
    exit 1
fi

echo "✓ Docker found"

# Build image
echo ""
echo "📦 Building DSVC image..."
docker build -t dsvc-node:latest . -q

echo "✓ Image built"

# Start cluster
echo ""
echo "🔧 Starting 3-node cluster..."
docker compose up -d

echo ""
echo "⏳ Waiting for cluster to be ready..."
sleep 8

# Health check
echo ""
echo "🏥 Health check..."
if curl -s http://localhost:8081/healthz > /dev/null 2>&1; then
    echo "✓ Cluster is healthy!"
else
    echo "⚠️  Cluster may still be starting. Run 'docker compose logs' to check."
fi

# Show status
echo ""
echo "📊 Cluster Status:"
curl -s http://localhost:8081/v1/cluster/status | python3 -m json.tool 2>/dev/null || curl -s http://localhost:8081/v1/cluster/status

echo ""
echo ""
echo "✅ Setup complete!"
echo ""
echo "Access points:"
echo "  • Node 1: http://localhost:8081"
echo "  • Node 2: http://localhost:8082"
echo "  • Node 3: http://localhost:8083"
echo ""
echo "Next steps:"
echo "  1. Upload a file:    curl -X PUT http://localhost:8081/v1/objects/test --data 'Hello DSVC!'"
echo "  2. Download it:      curl http://localhost:8081/v1/objects/test"
echo "  3. View logs:        docker compose logs -f"
echo "  4. Stop cluster:     docker compose down"
echo ""
