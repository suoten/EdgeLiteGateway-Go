# ── Stage 1: Frontend build ──────────────────────────────────────────
FROM node:18-alpine AS frontend-builder

ARG NPM_REGISTRY=https://registry.npmmirror.com

WORKDIR /build
COPY frontend/package.json frontend/package-lock.json* ./
RUN npm ci --registry $NPM_REGISTRY || npm install --registry $NPM_REGISTRY
COPY frontend/ ./
RUN npm run build

# ── Stage 2: Go backend build ────────────────────────────────────────
FROM golang:1.25-alpine AS backend-builder

WORKDIR /build

# Cache dependencies
COPY go.mod go.sum ./
RUN go mod download

# Copy source and build
COPY . .
COPY --from=frontend-builder /build/dist web/dist

RUN CGO_ENABLED=0 go build -ldflags="-s -w -X main.Version=1.0.0-go" -o /edgelite ./cmd/edgelite/

# ── Stage 3: Final runtime image ─────────────────────────────────────
FROM alpine:3.20

RUN apk add --no-cache ca-certificates tzdata wget \
    && addgroup -S appuser \
    && adduser -S -G appuser appuser

WORKDIR /app

# Copy binary
COPY --from=backend-builder /edgelite /app/edgelite

# Copy frontend dist
COPY --from=frontend-builder /build/dist /app/web/dist

# Copy config
COPY configs/ /app/configs/

# Create data directories
RUN mkdir -p data/backups data/logs \
    && chown -R appuser:appuser /app

USER appuser

ENV EDGELITE_CONFIG=configs/config.yaml
ENV EDGELITE_FRONTEND_DIST=/app/web/dist
ENV EDGELITE_SERVER__HOST=0.0.0.0
ENV EDGELITE_SERVER__PORT=8080

EXPOSE 8080

# Health check using /health/live lightweight endpoint
HEALTHCHECK --interval=30s --timeout=10s --retries=3 --start-period=30s \
    CMD wget --spider -q http://localhost:8080/health/live || exit 1

ENTRYPOINT ["/app/edgelite"]
CMD ["--host", "0.0.0.0", "--port", "8080"]
