# EdgeLite AI Sidecar

> Python HTTP JSON server wrapping ONNX Runtime for edge AI inference.
> Designed as a sidecar process for the EdgeLite Gateway (Go Edition).

[![CI](https://github.com/edgelite/edgelite-gateway/actions/workflows/ai-sidecar-ci.yml/badge.svg)](https://github.com/edgelite/edgelite-gateway/actions/workflows/ai-sidecar-ci.yml)
[![Python 3.11+](https://img.shields.io/badge/python-3.11+-blue.svg)](https://www.python.org/downloads/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Architecture

```
Go Gateway (8080)  ──── HTTP JSON (50052) ────>  Python AI Sidecar
  │                                              │
  ├── HTTP/API/WS                                ├── ONNX Runtime (CPU/CUDA/OpenVINO)
  ├── Device Collecting                          ├── Self-Learning (EWMA + Z-Score)
  ├── Rule Engine                                ├── Preset Model Generation (ONNX proto)
  └── Time-series Storage                        └── Prometheus Metrics (/metrics)
```

### Key Features

- **ONNX Inference** — Load, unload, hot-reload, and version-rollback ONNX models at runtime
- **Self-Learning** — Online anomaly detection using EWMA + Z-score with configurable thresholds
- **Preset Models** — Auto-generates 3 built-in ONNX models (anomaly, trend, threshold) on first start
- **Multi-Provider** — Supports CPU, CUDA, and OpenVINO execution providers with automatic fallback
- **Prometheus Metrics** — Native `/metrics` endpoint for monitoring (zero external dependency)
- **Health Probes** — Kubernetes-ready liveness (`/health/live`) and readiness (`/health/ready`) probes
- **Structured Logging** — Optional JSON log format via `AI_SIDECAR_JSON_LOGS=1`
- **Graceful Shutdown** — Signal-aware shutdown with scheduled task cleanup
- **Thread-Safe** — All shared state protected by locks; concurrent request safe
- **Error Standardization** — Consistent `error_code` + `error_message` dual-field error responses

## Quick Start

### Local Development

```bash
# Install dependencies
make ai-sidecar-dev-install

# Run in development mode (debug logs)
make ai-sidecar-run-dev

# Or run directly
cd ai_sidecar
python server.py --host 0.0.0.0 --port 50052 --models-dir models --log-level DEBUG
```

### Docker

```bash
# Build and start all services (including AI sidecar)
make docker-up

# View sidecar logs
make docker-logs-sidecar

# With monitoring stack (Prometheus + Grafana)
make docker-up-monitoring
```

### Testing

```bash
# Run all tests
make ai-sidecar-test

# Run with coverage report
make ai-sidecar-coverage

# Lint
make ai-sidecar-lint
```

## API Reference

### Health & Metrics

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Liveness probe (always 200 if process alive) |
| GET | `/health/live` | Alias for `/health` |
| GET | `/health/ready` | Readiness probe (200 if ≥1 active model, 503 otherwise) |
| GET | `/metrics` | Prometheus metrics (text exposition format) |

### Model Management

| Method | Path | Body | Description |
|--------|------|------|-------------|
| GET | `/models` | — | List all loaded models |
| POST | `/models/load` | `{model_id, model_path, model_type, ...}` | Load a new model |
| POST | `/models/unload` | `{model_id}` | Unload a model |
| POST | `/models/reload` | `{model_id, model_path?}` | Hot-reload a model (auto version bump) |
| POST | `/models/enable` | `{model_id}` | Enable a model (load if needed) |
| POST | `/models/disable` | `{model_id}` | Disable a model (unload) |
| POST | `/models/remove` | `{model_id}` | Remove a model permanently |
| GET | `/models/{model_id}/status` | — | Get model status |
| GET | `/models/{model_id}/stats` | — | Get model inference statistics |
| GET | `/models/{model_id}/history` | — | Get version history |
| POST | `/models/rollback` | `{model_id, target_version}` | Rollback to a previous version |
| POST | `/models/generate-presets` | — | Regenerate preset ONNX models |

### Inference

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/infer` | `{model_id, input_data: [float...]}` | Run inference |

### Scheduled Inference

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/scheduled/start` | `{model_id, device_id, point_name, interval_seconds, input_window_size}` | Start scheduled loop |
| POST | `/scheduled/stop` | `{model_id}` | Stop scheduled loop |

### Self-Learning

| Method | Path | Body | Description |
|--------|------|------|-------------|
| POST | `/self-learning/sample` | `{device_id, point_name, value, window_size?}` | Add sample, returns `is_anomaly` |
| POST | `/self-learning/predict` | `{device_id, point_name}` | Predict next value (EWMA) |
| POST | `/self-learning/stats` | `{device_id, point_name}` | Get model stats |
| GET | `/self-learning/stats/all` | — | Get all model stats |
| POST | `/self-learning/reset` | `{device_id, point_name}` | Reset a model |
| POST | `/self-learning/threshold` | `{device_id, point_name, threshold}` | Set anomaly threshold |

### Statistics & Providers

| Method | Path | Body | Description |
|--------|------|------|-------------|
| GET | `/stats` | — | Engine-wide statistics |
| GET | `/execution-providers` | — | List available providers |
| POST | `/execution-provider` | `{provider: "CPU"\|"CUDA"\|"OpenVINO"}` | Switch provider (reloads all models) |

### Error Response Format

All error responses follow a consistent dual-field format:

```json
{
  "success": false,
  "error_code": "ERR_AI_MODEL_NOT_FOUND",
  "error_message": "Model not found"
}
```

#### Error Code Reference

| Code | HTTP Status | Description |
|------|-------------|-------------|
| `ERR_INVALID_JSON` | 400 | Malformed JSON body |
| `ERR_INVALID_REQUEST` | 400 | Invalid request structure |
| `ERR_AI_MODEL_ID_REQUIRED` | 400 | `model_id` field missing |
| `ERR_AI_MODEL_ALREADY_LOADED` | 200 | Model ID already in memory |
| `ERR_AI_MODEL_NOT_FOUND` | 200 | Model ID not found |
| `ERR_AI_MODEL_NOT_AVAILABLE` | 200 | Model exists but not active |
| `ERR_AI_MODEL_FILE_NOT_FOUND` | 200 | ONNX file missing on disk |
| `ERR_AI_MODEL_IS_LOADING` | 200 | Model is currently loading |
| `ERR_AI_MODEL_CANNOT_LOAD` | 200 | Model load failed |
| `ERR_AI_INFERENCE_TIMEOUT` | 200 | Inference exceeded timeout |
| `ERR_AI_INFERENCE_FAILED` | 200 | Inference runtime error |
| `ERR_AI_INVALID_PROVIDER` | 400 | Invalid execution provider name |
| `ERR_AI_ROLLBACK_FAILED` | 200 | Version rollback failed |
| `ERR_AI_VERSION_NOT_FOUND` | 200 | Target version not in history |
| `ERR_ROUTE_NOT_FOUND` | 404 | Unknown API route |
| `ERR_METHOD_NOT_ALLOWED` | 405 | Wrong HTTP method |
| `ERR_INTERNAL_SERVER_ERROR` | 500 | Unhandled exception |

## Prometheus Metrics

The `/metrics` endpoint exposes the following metrics:

| Metric | Type | Description |
|--------|------|-------------|
| `ai_sidecar_inference_total` | counter | Total inference calls |
| `ai_sidecar_inference_errors` | counter | Total inference errors |
| `ai_sidecar_avg_latency_ms` | gauge | Average inference latency (ms) |
| `ai_sidecar_models_loaded` | gauge | Number of loaded models |
| `ai_sidecar_uptime_seconds` | gauge | Server uptime (seconds) |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `AI_SIDECAR_HOST` | `0.0.0.0` | Listen host |
| `AI_SIDECAR_PORT` | `50052` | Listen port |
| `AI_MODELS_DIR` | `models` | Directory for ONNX model files |
| `AI_SIDECAR_LOG_LEVEL` | `INFO` | Log level (`DEBUG`, `INFO`, `WARNING`, `ERROR`) |
| `AI_SIDECAR_JSON_LOGS` | `0` | Set to `1` for structured JSON logs |

## Preset Models

Three built-in ONNX models are auto-generated on first startup:

| Model ID | Type | Input | Output | Description |
|----------|------|-------|--------|-------------|
| `preset-anomaly-v1` | anomaly | `[1, 100]` float32 | `[1]` float32 | Anomaly score 0-1 (Sigmoid) |
| `preset-trend-v1` | trend | `[1, 200]` float32 | `[1, 10]` float32 | Next 10-step prediction |
| `preset-threshold-v1` | threshold | `[1, 50]` float32 | `[1]` float32 | Optimal dynamic threshold |

## Project Structure

```
ai_sidecar/
├── server.py              # Main HTTP server (aiohttp)
├── requirements.txt       # Production dependencies
├── requirements-dev.txt   # Development dependencies (pytest, flake8, mypy)
├── pyproject.toml         # Project config (build, pytest, flake8, mypy, coverage)
├── README.md              # This file
├── tests/
│   ├── conftest.py        # pytest fixtures
│   ├── test_self_learning.py    # SelfLearningModel/Manager unit tests
│   ├── test_stats_collector.py  # InferenceStatsCollector unit tests
│   └── test_api.py        # HTTP API integration tests
└── models/                # Generated ONNX models (auto-created)
```

## Development

### Running Tests

```bash
cd ai_sidecar

# Install dev dependencies
pip install -r requirements-dev.txt

# Run all tests
pytest tests/ -v

# Run with coverage
pytest tests/ -v --cov=server --cov-report=term-missing

# Run specific test class
pytest tests/test_self_learning.py::TestSelfLearningModel -v
```

### Code Quality

```bash
# Lint
flake8 server.py tests/ --max-line-length=120 --extend-ignore=E203,W503

# Type checking
mypy server.py --ignore-missing-imports
```

### CLI Arguments

```bash
python server.py --help
# usage: server.py [-h] [--host HOST] [--port PORT]
#                   [--models-dir MODELS_DIR] [--log-level LEVEL]
#                   [--json-logs]
#
# EdgeLite AI Sidecar (HTTP)
#
# options:
#   --host          Listen host (default: 0.0.0.0)
#   --port          Listen port (default: 50052)
#   --models-dir    ONNX models directory (default: models)
#   --log-level     Log level (default: INFO)
#   --json-logs     Enable structured JSON logging
```

## License

MIT
