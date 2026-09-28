#!/usr/bin/env bash
# EdgeLite AI Sidecar startup script
# Usage: ./scripts/start_sidecar.sh [--dev]
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
SIDECAR_DIR="$PROJECT_ROOT/ai_sidecar"

# Default configuration
HOST="${AI_SIDECAR_HOST:-0.0.0.0}"
PORT="${AI_SIDECAR_PORT:-50052}"
MODELS_DIR="${AI_MODELS_DIR:-$SIDECAR_DIR/models}"
LOG_LEVEL="INFO"
JSON_LOGS=""

# Parse arguments
if [[ "${1:-}" == "--dev" ]]; then
  LOG_LEVEL="DEBUG"
  echo "Starting in DEVELOPMENT mode..."
fi

# Create models directory if it doesn't exist
mkdir -p "$MODELS_DIR"

# Check Python is available
if ! command -v python &> /dev/null; then
  echo "ERROR: python is not installed or not in PATH"
  exit 1
fi

# Check dependencies
if ! python -c "import aiohttp, onnxruntime, numpy" 2>/dev/null; then
  echo "Installing dependencies..."
  cd "$SIDECAR_DIR"
  pip install -r requirements.txt
fi

# Health check function
check_health() {
  local max_retries=30
  local retry=0
  while [[ $retry -lt $max_retries ]]; do
    if curl -s "http://$HOST:$PORT/health/live" | grep -q '"healthy":true' 2>/dev/null; then
      echo "✅ AI Sidecar is healthy (attempt $((retry+1)))"
      return 0
    fi
    retry=$((retry+1))
    sleep 1
  done
  echo "❌ AI Sidecar failed to become healthy within ${max_retries}s"
  return 1
}

# Start server
echo "Starting EdgeLite AI Sidecar on $HOST:$PORT"
echo "  Models dir: $MODELS_DIR"
echo "  Log level:  $LOG_LEVEL"

cd "$SIDECAR_DIR"
python server.py \
  --host "$HOST" \
  --port "$PORT" \
  --models-dir "$MODELS_DIR" \
  --log-level "$LOG_LEVEL" \
  ${JSON_LOGS:+--json-logs} &
SIDECAR_PID=$!

echo "  PID: $SIDECAR_PID"

# Wait for health
if check_health; then
  echo "Server is ready at http://$HOST:$PORT"
  echo "  Health:  http://$HOST:$PORT/health"
  echo "  Metrics: http://$HOST:$PORT/metrics"
  echo "  Models:  http://$HOST:$PORT/models"
else
  echo "WARNING: Health check failed, server may still be starting..."
fi

# Wait for process to exit
wait $SIDECAR_PID
