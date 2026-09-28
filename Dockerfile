# EdgeLite Gateway - Unified Dockerfile
# Multi-stage build for the Go gateway with embedded frontend
# For the AI Sidecar Dockerfile, see docker/Dockerfile.sidecar

# ── Builder stage ──────────────────────────────────────────────────
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /edgelite ./cmd/edgelite

# ── Frontend stage ─────────────────────────────────────────────────
FROM node:20-alpine AS frontend

WORKDIR /frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --production

COPY frontend/ .
RUN npm run build

# ── Runtime stage ──────────────────────────────────────────────────
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -h /app edgelite

WORKDIR /app

COPY --from=builder /edgelite .
COPY --from=frontend /frontend/dist ./web/dist
COPY configs/ ./configs/

USER edgelite

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --retries=3 --start-period=30s \
    CMD wget --spider -q http://localhost:8080/health/live || exit 1

ENTRYPOINT ["./edgelite"]
