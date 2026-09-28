# Changelog

All notable changes to the EdgeLite Gateway (Go Edition) project are documented
in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [1.0.0] - 2024-01-15

### Added
- Go gateway with Echo framework (HTTP REST API on port 8080)
- AI Sidecar (Python HTTP JSON server on port 50052) with ONNX Runtime
- Frontend SPA (Vue 3 + Element Plus + TypeScript)
- Docker Compose stack (gateway, sidecar, InfluxDB, Mosquitto, Prometheus, Grafana)
- Self-learning anomaly detection (EWMA + Z-score)
- Model lifecycle management (load, unload, reload, enable, disable, remove, rollback)
- Version history tracking with auto version bump
- Scheduled inference loops
- Execution provider switching (CPU, CUDA, OpenVINO)
- Prometheus metrics exposition (`/metrics` endpoint)
- Health checks (`/health/live`, `/health/ready`)
- Structured JSON logging
- Comprehensive test suite (211 tests, 91.74% coverage)
- OpenAPI 3.0 specification (`ai_sidecar/openapi.yaml`)
- Kubernetes manifests (`deploy/k8s/ai-sidecar.yaml`)
- Full-stack CI/CD pipeline (`.github/workflows/ci-cd.yml`)
- Benchmark script (`ai_sidecar/benchmark.py`)
- Operations runbook (`docs/runbook.md`)
- Startup scripts (`scripts/start_sidecar.sh`, `scripts/start_sidecar.ps1`)

### Security
- Non-root container user
- `cap_drop: ALL` and `no-new-privileges` in Docker
- Resource limits on all containers
- Secret management via environment variables
- CORS configuration
- Authentication and authorization middleware

### Changed
- Migrated from Python gateway to Go (Echo framework)
- AI inference moved to separate Python sidecar process
- Frontend rebuilt with Vue 3 + TypeScript

### Deprecated
- Python gateway (replaced by Go edition)

### Removed
- Legacy Python gateway code

### Fixed
- Concurrency safety in SelfLearningModel (RLock)
- Inference timeout fallback to cached results
- Model wrapper initialization with optional schemas
- mypy strict mode compliance (0 errors)
- flake8 compliance (0 warnings)
