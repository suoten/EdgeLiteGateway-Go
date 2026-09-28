#!/bin/bash
# EdgeLite Gateway (Go Edition) - Startup Script
# Usage: ./start.sh [--host HOST] [--port PORT] [--config CONFIG_PATH]

set -e

# Default values
HOST="${EDGELITE_SERVER__HOST:-0.0.0.0}"
PORT="${EDGELITE_SERVER__PORT:-8080}"
CONFIG="${EDGELITE_CONFIG:-configs/config.yaml}"

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --host)
            HOST="$2"
            shift 2
            ;;
        --port)
            PORT="$2"
            shift 2
            ;;
        --config)
            CONFIG="$2"
            shift 2
            ;;
        --help|-h)
            echo "EdgeLite Gateway (Go Edition)"
            echo ""
            echo "Usage: $0 [OPTIONS]"
            echo ""
            echo "Options:"
            echo "  --host HOST       Listen address (default: 0.0.0.0)"
            echo "  --port PORT       Listen port (default: 8080)"
            echo "  --config PATH     Config file (default: configs/config.yaml)"
            echo "  --help, -h        Show this help"
            echo ""
            echo "Environment variables:"
            echo "  EDGELITE_SERVER__HOST    Override listen address"
            echo "  EDGELITE_SERVER__PORT    Override listen port"
            echo "  EDGELITE_CONFIG          Override config path"
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Check if binary exists, build if not
if [ ! -f "./edgelite" ] && [ ! -f "./edgelite.exe" ]; then
    echo "Building EdgeLite Gateway..."
    go build -o edgelite ./cmd/edgelite
fi

# Determine binary name
if [ -f "./edgelite" ]; then
    BINARY="./edgelite"
elif [ -f "./edgelite.exe" ]; then
    BINARY="./edgelite.exe"
else
    echo "ERROR: Binary not found. Run 'go build -o edgelite ./cmd/edgelite' first."
    exit 1
fi

echo "Starting EdgeLite Gateway..."
echo "  Host: $HOST"
echo "  Port: $PORT"
echo "  Config: $CONFIG"
echo ""

exec "$BINARY" --host "$HOST" --port "$PORT" --config "$CONFIG"
