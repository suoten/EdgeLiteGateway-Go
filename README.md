# EdgeLiteGateway-Go

> Industrial-grade edge computing gateway with AI inference, device collection,
> rule engine, and time-series storage. Built in Go with a Python AI sidecar.
>
> 工业级边缘计算网关（Go 社区版）：多协议采集 · AI 推理 · 规则引擎 · 时序存储 · 3D 数字孪生。

[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](https://go.dev/)
[![Python](https://img.shields.io/badge/AI_Sidecar-3.11+-blue.svg)](https://www.python.org/)
[![Vue](https://img.shields.io/badge/Frontend-Vue_3-42B883.svg)](https://vuejs.org/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)

## Why Go Edition / 为什么选 Go 版

与 [Python 社区版](https://gitee.com/suoten/EdgeLiteGateway)功能同级，但：

- **单二进制部署** — 编译后无运行时依赖，x64/arm64/armv7 三架构开箱即用（见 [release/](release/)）
- **更高采集吞吐** — Go 协程原生并发，万级测点采集占用更低内存
- **低配设备友好** — 工控机、边缘盒子（ARM）资源占用显著低于 Python 版
- **同一套前端 / API** — Vue 3 前端与 REST API 与 Python 版同构，迁移零成本

> 🐍 追求 10 分钟零门槛 Docker 体验、或需要改 Python 源码做二次开发？请用 [Python 版](https://github.com/suoten/EdgeLiteGateway)。

## Releases / 预编译下载

`release/` 目录随仓库提供 v1.0.0 预编译二进制（Linux amd64 / arm64 / armv7）：

```bash
# 校验后解压即用（内置 install.sh）
sha256sum -c checksums.txt
tar -xzf edgelite-1.0.0-linux-amd64.tar.gz
cd edgelite-1.0.0 && ./install.sh
```

也可自行从源码构建：`make all`。

## Overview

EdgeLite Gateway is an industrial IoT edge computing platform that provides:

- **Multi-Protocol Device Collection** — Modbus TCP/RTU, Modbus Slave (built-in simulator), OPC UA, Siemens S7, Mitsubishi MC, Omron FINS, Allen-Bradley (CIP), ONVIF, MQTT, HTTP — 10 protocol drivers with circuit breaker, health monitoring and auto-reconnect
- **AI Inference** — ONNX Runtime with Python sidecar (anomaly detection, trend prediction, threshold optimization)
- **Rule Engine** — Threshold, AI inference, and script-based rules with alarm management
- **Time-Series Storage** — InfluxDB with SQLite fallback and automatic downsampling
- **Northbound Platform** — MQTT/HTTP forwarding to ThingsBoard, custom platforms
- **Security** — JWT auth, RBAC, rate limiting, CSRF protection, encrypted secrets
- **Observability** — Health probes, Prometheus metrics, structured logging, message tracing
- **Digital Twin** — 3D visualization with SCADA-style HMI

## Related Projects / 相关项目

| Project | Description |
|---------|-------------|
| [EdgeLiteGateway (Python)](https://gitee.com/suoten/EdgeLiteGateway) · [GitHub](https://github.com/suoten/EdgeLiteGateway) | Python 社区版：13 种工业协议 + ONNX，10 分钟 Docker 部署，源码级二开首选 |
| [ProtoForge](https://gitee.com/suoten/ProtoForge) | 开源 PLC 协议模拟器，与 EdgeLite 联调采集/下写链路 |

## Architecture

```
                    ┌─────────────────────────────────────────────────┐
                    │              EdgeLite Gateway (Go)                │
                    │                                                   │
  Devices ────────► │  Drivers ──► Preprocessor ──► Rule Engine         │
  (Modbus, OPC,     │                   │              │                │
  S7, MC, FINS,     │                   ▼              ▼                │
  AB, ONVIF,        │             Cache/Ring      Alarm Manager         │
  MQTT, HTTP)       │                   │              │                │
                    │             InfluxDB/SQLite    Notifications      │
                    │                   │              │                │
                    │             Northbound ────► Platform             │
                    │                                                   │
                    │  HTTP API (8080) ◄── Frontend (Vue 3)             │
                    └────────────────┬──────────────────────────────────┘
                                     │ HTTP JSON (50052)
                    ┌────────────────▼──────────────────────────────────┐
                    │           AI Sidecar (Python)                      │
                    │  ONNX Runtime │ Self-Learning │ Preset Models      │
                    │  Prometheus /metrics │ Health Probes               │
                    └───────────────────────────────────────────────────┘
```

## Quick Start

### Prerequisites

- Go 1.25+
- Python 3.11+ (for AI sidecar)
- Docker & Docker Compose (recommended)

### Option 1: Docker Compose (Recommended)

```bash
# Clone and configure
git clone https://gitee.com/suoten/EdgeLiteGateway-Go.git
cd EdgeLiteGateway-Go
cp docker/.env.example docker/.env
# Edit docker/.env with your secrets!

# Start all services
make docker-up

# With monitoring stack (Prometheus + Grafana)
make docker-up-monitoring

# View logs
make docker-logs
```

### Option 2: Local Development

```bash
# Install AI sidecar dependencies
make ai-sidecar-dev-install

# Start AI sidecar (terminal 1)
make ai-sidecar-run

# Build and run Go gateway (terminal 2)
make go-build
./edgelite.exe

# Or run directly
make go-run
```

### Option 3: Make All

```bash
make all  # Install deps + build
```

## Project Structure

```
EdgeLiteGateway-Go/
├── cmd/edgelite/           # Go main entrypoint
├── internal/
│   ├── api/                # HTTP API handlers (Echo)
│   ├── config/             # YAML config with hot-reload
│   ├── constants/          # Global constants
│   ├── drivers/            # Protocol drivers (Modbus, OPC UA, S7, ...)
│   ├── engine/             # Core engine (AI, rules, scheduler, events)
│   ├── middleware/         # HTTP middleware (auth, logging, CORS)
│   ├── models/             # Data models
│   ├── platform/           # Northbound platform connectors
│   ├── security/           # Auth, RBAC, rate limiting
│   ├── services/           # Business services
│   ├── storage/            # Database, repositories, TS cache
│   └── ws/                 # WebSocket manager
├── ai_sidecar/             # Python AI sidecar (ONNX inference)
│   ├── server.py           # HTTP JSON server
│   ├── tests/              # pytest unit + integration tests
│   ├── requirements.txt    # Production deps
│   ├── requirements-dev.txt # Dev deps (pytest, flake8, mypy)
│   └── pyproject.toml      # Project config
├── configs/                # YAML configuration
├── deploy/
│   ├── helm/               # Helm Chart (K8s deployment)
│   │   ├── Chart.yaml
│   │   ├── values.yaml
│   │   └── templates/      # Deployment, Service, Ingress, HPA, PDB, etc.
│   └── k8s/                # Raw K8s manifests
├── docker/                 # Docker configs (Dockerfile, compose, prometheus)
├── frontend/               # Vue 3 + Naive UI frontend
├── tests/
│   └── integration/        # Go integration tests (28 tests)
├── release/                # Prebuilt binaries (linux amd64/arm64/armv7) + checksums
├── Makefile                # Unified build/test/run commands
└── go.mod                  # Go module definition
```

## Key Components

### AI Sidecar (`ai_sidecar/`)

Python HTTP JSON server wrapping ONNX Runtime. See [ai_sidecar/README.md](ai_sidecar/README.md) for full API reference.

- 3 preset ONNX models auto-generated on startup
- Self-learning anomaly detection (EWMA + Z-score)
- Prometheus `/metrics` endpoint
- Kubernetes-ready liveness/readiness probes
- 59 unit + integration tests (100% pass rate)

### Device Drivers (`internal/drivers/`)

10 protocol drivers (Modbus TCP/RTU/Slave, OPC UA, S7, Mitsubishi MC, Omron FINS, Allen-Bradley CIP, ONVIF, MQTT, HTTP) with circuit breaker, health monitoring, and automatic reconnection.

### Rule Engine (`internal/engine/`)

Threshold, AI inference, and script-based rules with alarm correlation, deduplication, and notification channels (DingTalk, Email, WeChat, Webhook).

### Configuration (`internal/config/`)

YAML-based with environment variable interpolation, `.env` overrides, hot-reload with change detection, and sensitive field encryption.

## Testing

```bash
# AI sidecar tests
make ai-sidecar-test

# AI sidecar with coverage
make ai-sidecar-coverage

# Go unit tests
make go-test

# Go integration tests (28 tests covering full API lifecycle)
go test ./tests/integration/ -v -count=1

# Static analysis
go vet ./...
go build ./...

# Lint
make ai-sidecar-lint
make go-lint
```

### Integration Test Coverage

The integration test suite (`tests/integration/`) verifies:

| Category | Tests | Description |
|----------|-------|-------------|
| Health | 2 | Liveness + readiness probes |
| Auth | 6 | Login, invalid credentials, empty body, current user, no auth |
| Device CRUD | 5 | Create, list, get, update, delete, not-found |
| Authorization | 4 | Viewer cannot create, viewer can read, no auth, invalid token |
| Rule CRUD | 2 | Create+list, delete |
| Security | 2 | Security headers, request ID |
| API Format | 1 | Standard response format validation |
| Database | 1 | Health after multiple operations |
| Concurrency | 1 | Sequential device creation integrity |
| Edge Cases | 3 | Malformed JSON, empty device ID, password change flow |
| Auth Flow | 2 | Logout, token refresh |
| Metrics | 1 | Prometheus metrics endpoint |
| Rate Limiting | 1 | 4th request blocked |

## Kubernetes Deployment (Helm)

```bash
# Deploy via Helm Chart
helm install edgelite deploy/helm/ \
  --set gateway.security.secretKey=<your-32+char-key> \
  --set gateway.security.csrfSecret=<your-csrf-secret> \
  --namespace edgelite --create-namespace

# Upgrade
helm upgrade edgelite deploy/helm/ \
  --set gateway.security.secretKey=<your-key> \
  --namespace edgelite

# With Ingress and TLS
helm install edgelite deploy/helm/ \
  --set ingress.enabled=true \
  --set ingress.hosts[0].host=edgelite.example.com \
  --set ingress.tls[0].hosts[0]=edgelite.example.com \
  --namespace edgelite

# Uninstall
helm uninstall edgelite --namespace edgelite
```

### Helm Chart Features

| Feature | Description |
|---------|-------------|
| Deployment | Rolling update with init container for DB dir |
| Service | ClusterIP / NodePort configurable |
| Ingress | nginx ingress with TLS support |
| ConfigMap | YAML config rendered from values |
| Secret | Auto-generated secretKey + csrfSecret |
| PVC | Persistent volume for SQLite data |
| HPA | CPU/Memory-based autoscaling |
| PDB | Pod disruption budget |
| AI Sidecar | Separate Deployment + Service + PVC for models |
| NetworkPolicy | Optional ingress/egress isolation |
| ServiceAccount | With configurable automount |
| Probes | Liveness + readiness HTTP probes |

## Configuration

See `configs/config.yaml` for all options. Key sections:

| Section | Description |
|---------|-------------|
| `server` | HTTP host, port, CORS, debug |
| `database` | SQLite/MySQL backend, pool, backup |
| `influxdb` | Time-series storage with fallback |
| `mqtt` | Southbound MQTT client |
| `security` | JWT, RBAC, rate limiting, CSRF |
| `ai_inference` | Sidecar URL, timeout, concurrency |
| `notify` | DingTalk, Email, WeChat, Webhook |
| `backup` | Automatic backup scheduler |

### Environment Variables

All YAML config can be overridden via `EDGELITE_<SECTION>__<KEY>` env vars:

```bash
EDGELITE_SERVER__PORT=9090
EDGELITE_SECURITY__SECRET_KEY=my-secret
EDGELITE_AI_INFERENCE__SIDECAR_URL=http://ai-sidecar:50052
```

## Docker Services

| Service | Port | Description |
|---------|------|-------------|
| `edgelite` | 8080 | Go gateway |
| `ai-sidecar` | 50052 | Python ONNX inference |
| `influxdb` | 8086 | Time-series database |
| `mosquitto` | 1883 | MQTT broker |
| `prometheus` | 9090 | Metrics scraper (profile: monitoring) |
| `grafana` | 3001 | Dashboards (profile: monitoring) |

## CI/CD

GitHub Actions pipeline (`.github/workflows/ai-sidecar-ci.yml`):
- Flake8 lint + MyPy type check
- pytest with coverage (59 tests)
- Docker image build verification

## License

MIT — free for commercial use. 商业授权、定制协议驱动与企业版（更多协议与高级特性）请联系作者。

## Support / 支持

如果这个项目对你有帮助，欢迎 Star ⭐ 与反馈 Issue，这是社区版持续维护的最大动力。

## API Reference

### Standard Response Format

All API endpoints return a standard JSON response:

```json
{
  "code": 0,
  "message": "success",
  "data": { },
  "error_code": ""
}
```

Paged responses include `total`, `page`, `size` fields.

### Authentication

| Method | Endpoint | Description | Auth |
|--------|----------|-------------|------|
| POST | `/api/v1/auth/login` | Login with username/password | No |
| POST | `/api/v1/auth/refresh` | Refresh access token | No |
| POST | `/api/v1/auth/logout` | Revoke current session | Yes |
| GET | `/api/v1/auth/me` | Get current user info | Yes |
| POST | `/api/v1/auth/change-password` | Change password | Yes |
| POST | `/api/v1/auth/forgot-password` | Request password reset | No |
| POST | `/api/v1/auth/reset-password` | Reset password with token | No |

### Devices

| Method | Endpoint | Permission | Description |
|--------|----------|------------|-------------|
| GET | `/api/v1/devices` | `device:read` | List devices (paged) |
| POST | `/api/v1/devices` | `device:create` | Create device |
| GET | `/api/v1/devices/:id` | `device:read` | Get device by ID |
| PUT | `/api/v1/devices/:id` | `device:update` | Update device |
| DELETE | `/api/v1/devices/:id` | `device:delete` | Delete device |
| GET | `/api/v1/devices/:id/points` | `device:read` | Get device points |
| POST | `/api/v1/devices/:id/points` | `device:write` | Write device point |
| GET | `/api/v1/devices/:id/health` | `device:read` | Get device health |
| POST | `/api/v1/devices/export` | `device:read` | Export devices |
| POST | `/api/v1/devices/import` | `device:create` | Import devices |

### Rules

| Method | Endpoint | Permission | Description |
|--------|----------|------------|-------------|
| GET | `/api/v1/rules` | `rule:read` | List rules (paged) |
| POST | `/api/v1/rules` | `rule:create` | Create rule |
| GET | `/api/v1/rules/:id` | `rule:read` | Get rule by ID |
| PUT | `/api/v1/rules/:id` | `rule:update` | Update rule |
| DELETE | `/api/v1/rules/:id` | `rule:delete` | Delete rule |
| POST | `/api/v1/rules/:id/enable` | `rule:read` | Enable rule |
| POST | `/api/v1/rules/:id/disable` | `rule:read` | Disable rule |

### Alarms

| Method | Endpoint | Permission | Description |
|--------|----------|------------|-------------|
| GET | `/api/v1/alarms` | `alarm:read` | List alarms (paged) |
| GET | `/api/v1/alarms/:id` | `alarm:read` | Get alarm by ID |
| POST | `/api/v1/alarms/:id/ack` | `alarm:ack` | Acknowledge alarm |

### Data

| Method | Endpoint | Permission | Description |
|--------|----------|------------|-------------|
| GET | `/api/v1/data/:device_id/latest` | `data:read` | Get latest data |
| GET | `/api/v1/data/:device_id/history` | `data:read` | Get historical data |
| POST | `/api/v1/data/export` | `data:export` | Export data |

### System

| Method | Endpoint | Permission | Description |
|--------|----------|------------|-------------|
| GET | `/api/v1/system/info` | — | System info |
| GET | `/api/v1/system/config` | `system:config` | Get system config |
| GET | `/api/v1/system/health` | — | System health |

### Health & Metrics

| Method | Endpoint | Auth | Description |
|--------|----------|------|-------------|
| GET | `/health/live` | No | Liveness probe |
| GET | `/health/ready` | No | Readiness probe |
| GET | `/metrics` | No | Prometheus metrics |

### RBAC Roles

| Role | Permissions |
|------|-------------|
| `admin` | Full access (all CRUD on devices, rules, alarms, users, system) |
| `operator` | Read+update devices, full rule CRUD, alarm read+ack, data read+export |
| `viewer` | Read-only access to devices, rules, alarms, data |

## Testing

### Go Gateway Tests

```bash
# Unit tests
make go-test

# Integration tests (28 tests)
make go-test-integration

# All tests with coverage
make go-test-all

# E2E smoke tests (in-process, no external server needed)
go test ./tests/e2e/ -v -count=1 -timeout 120s

# Generate coverage report
go test ./internal/... -coverprofile=coverage.out -covermode=atomic
go tool cover -func=coverage.out
go tool cover -html=coverage.out -o coverage.html
```

### Test Coverage by Module

| Module | Coverage | Description |
|--------|----------|-------------|
| `internal/security` | 91.3% | JWT, RBAC, password hashing, CSRF |
| `internal/models` | 88.9% | Data models and validation |
| `internal/ws` | 43.4% | WebSocket manager and channels |
| `internal/config` | 38.9% | Configuration loading and hot-reload |
| `internal/storage` | 20.6% | Database, repositories, TS cache |
| `internal/middleware` | 25.1% | Auth, logging, CORS, rate limiting |
| `internal/engine` | 10.0% | Event bus, scheduler, rule evaluator |
| `internal/constants` | 100% | Protocol normalization, constants |

### AI Sidecar Tests

```bash
make ai-sidecar-test           # Run pytest
make ai-sidecar-coverage       # With coverage (≥90% threshold)
make ai-sidecar-strict         # flake8 + mypy + pytest
```

## Helm Chart Deployment

```bash
# Lint the chart
helm lint deploy/helm/

# Template render
helm template deploy/helm/ --debug

# Package
helm package deploy/helm/ --destination ./

# Install
helm upgrade --install edgelite deploy/helm/ \
  --namespace edgelite \
  --create-namespace \
  --set image.gateway.tag=v1.0.0 \
  --set image.sidecar.tag=v1.0.0

# Uninstall
helm uninstall edgelite -n edgelite
```

### Helm Values

| Key | Default | Description |
|-----|---------|-------------|
| `image.gateway.repository` | `edgelite-gateway` | Gateway image |
| `image.gateway.tag` | `latest` | Gateway image tag |
| `image.sidecar.repository` | `edgelite-ai-sidecar` | AI sidecar image |
| `image.sidecar.tag` | `latest` | AI sidecar tag |
| `gateway.replicas` | `1` | Gateway replicas |
| `gateway.resources` | see values | CPU/memory limits |
| `sidecar.replicas` | `1` | AI sidecar replicas |
| `ingress.enabled` | `false` | Enable ingress |
| `autoscaling.enabled` | `false` | Enable HPA |

## CI/CD Pipeline

The GitHub Actions pipeline (`.github/workflows/ci-cd.yml`) includes:

1. **Go Tests** — Unit + integration tests with coverage report
2. **Go Lint** — `go vet` + `golangci-lint` with all enabled linters
3. **AI Sidecar Tests** — flake8 + mypy + pytest with ≥90% coverage
4. **Docker Build** — Multi-stage builds for gateway and sidecar
5. **K8s Validation** — Helm lint + template render
6. **Deploy** — Helm chart package (on main branch push)

## Configuration

See `configs/config.yaml` for all configuration options. Key sections:

- `server` — Host, port, CORS
- `database` — SQLite path, backup dir
- `security` — JWT secret, CSRF secret, token expiry
- `scheduler` — Collection intervals
- `mqtt` — Broker config, offline queue
- `ai_inference` — Sidecar URL, max concurrent
- `notify` — DingTalk, WeCom, Email, Webhook
