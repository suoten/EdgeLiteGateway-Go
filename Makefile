# EdgeLite Gateway (Go Edition) - Unified Makefile
# Usage: make [target]

.PHONY: ai-sidecar-install ai-sidecar-dev-install ai-sidecar-lint ai-sidecar-test \
        ai-sidecar-run ai-sidecar-run-dev ai-sidecar-coverage ai-sidecar-benchmark ai-sidecar-strict \
        go-build go-run go-test go-test-coverage go-test-integration go-test-e2e go-test-all go-lint go-vet \
        frontend-build frontend-dev \
        docker-build docker-up docker-down docker-logs \
        k8s-deploy k8s-logs \
        all clean coverage

# ── AI Sidecar (Python) ────────────────────────────────────────────

ai-sidecar-install:
	cd ai_sidecar && pip install -r requirements.txt

ai-sidecar-dev-install:
	cd ai_sidecar && pip install -r requirements-dev.txt

ai-sidecar-lint:
	cd ai_sidecar && flake8 server.py tests/ --max-line-length=120 --extend-ignore=E203,W503
	cd ai_sidecar && mypy server.py --strict --ignore-missing-imports

ai-sidecar-test:
	cd ai_sidecar && pytest tests/ -v --tb=short

ai-sidecar-coverage:
	cd ai_sidecar && pytest tests/ -v --tb=short --cov=server --cov-report=term-missing --cov-fail-under=90

ai-sidecar-benchmark:
	cd ai_sidecar && python benchmark.py --host localhost --port 50052 --concurrent 10 --requests 100

ai-sidecar-strict:
	cd ai_sidecar && flake8 server.py tests/ --max-line-length=120 --extend-ignore=E203,W503
	cd ai_sidecar && mypy server.py --strict --ignore-missing-imports
	cd ai_sidecar && pytest tests/ --cov=server --cov-report=term-missing --cov-fail-under=90 -q

ai-sidecar-run:
	cd ai_sidecar && python server.py --host 0.0.0.0 --port 50052 --models-dir models

ai-sidecar-run-dev:
	cd ai_sidecar && python server.py --host 0.0.0.0 --port 50052 --models-dir models --log-level DEBUG

# ── Go Gateway ─────────────────────────────────────────────────────

go-build:
	go build -ldflags="-s -w -X main.Version=1.0.0-go" -o edgelite ./cmd/edgelite

# ── Linux 交叉编译（CGO_ENABLED=0 纯 Go 静态二进制） ────────────────
# arm7: Cortex-A7/A8/A9 等 ARMv7 设备；amd64: x86_64 服务器；arm64: 64位 ARM

go-build-linux-arm7:
	GOOS=linux GOARCH=arm GOARM=7 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/edgelite-linux-arm7 ./cmd/edgelite

go-build-linux-amd64:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/edgelite-linux-amd64 ./cmd/edgelite

go-build-linux-arm64:
	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -ldflags="-s -w" -o dist/edgelite-linux-arm64 ./cmd/edgelite

go-build-linux-all: go-build-linux-arm7 go-build-linux-amd64 go-build-linux-arm64

frontend-build:
	cd frontend && npm ci && npm run build

frontend-dev:
	cd frontend && npm run dev

go-run: go-build
	./edgelite

go-test:
	go test ./internal/... -v -count=1 -timeout 120s

go-test-coverage:
	go test ./internal/... -count=1 -timeout 120s -coverprofile=coverage.out -covermode=atomic
	go tool cover -func=coverage.out | tail -1
	go tool cover -html=coverage.out -o coverage.html

go-test-integration:
	go test ./tests/integration/ -v -count=1 -timeout 120s

go-test-e2e:
	go test ./tests/e2e/ -v -count=1 -timeout 120s -run TestSmokeApp

go-test-all:
	go test ./... -v -count=1 -timeout 120s

go-vet:
	go vet ./...

go-lint:
	golangci-lint run ./internal/... || go vet ./internal/...

# ── Docker ─────────────────────────────────────────────────────────

docker-build:
	docker compose -f docker/docker-compose.yml build

docker-up:
	docker compose -f docker/docker-compose.yml up -d

docker-up-monitoring:
	docker compose -f docker/docker-compose.yml --profile monitoring up -d

docker-down:
	docker compose -f docker/docker-compose.yml down

docker-logs:
	docker compose -f docker/docker-compose.yml logs -f --tail=100

docker-logs-sidecar:
	docker compose -f docker/docker-compose.yml logs -f --tail=100 ai-sidecar

# ── Kubernetes ────────────────────────────────────────────────────

k8s-deploy:
	kubectl apply -f deploy/k8s/ai-sidecar.yaml

k8s-logs:
	kubectl logs -n edgelite -l app.kubernetes.io/name=ai-sidecar -f --tail=100

# ── Composite targets ──────────────────────────────────────────────

all: ai-sidecar-dev-install frontend-build go-build

coverage: go-test-coverage

clean:
	rm -f edgelite edgelite.exe~ coverage.out coverage.html
	rm -rf ai_sidecar/__pycache__ ai_sidecar/tests/__pycache__
	rm -rf ai_sidecar/.pytest_cache ai_sidecar/.mypy_cache
