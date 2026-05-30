# Stage 1: Build the frontend
FROM node:20-alpine AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN npm install
COPY frontend/ ./
RUN npm run build

# Stage 2: Build the backend
FROM golang:1.25-alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN go install github.com/swaggo/swag/cmd/swag@latest
RUN swag init -g main.go --parseDependency --parseInternal
RUN go build -o baki-backend main.go

# Stage 3: Final lean image
FROM alpine:latest
# ca-certificates: HTTPS to AI providers
# libc6-compat:    glibc compat for the Go binary
# wget:            HEALTHCHECK probe (alpine doesn't ship curl by default)
RUN apk add --no-cache ca-certificates libc6-compat wget

# Run as non-root. A compromised app process cannot then modify system files
# or escape to other tenants on a shared host.
RUN addgroup -g 1000 -S pad && adduser -u 1000 -S pad -G pad

WORKDIR /app
COPY --from=backend-builder --chown=pad:pad /app/baki-backend .
COPY --from=frontend-builder --chown=pad:pad /app/frontend/dist ./frontend/dist

# Default environment variables for cloud-style deployment.
# NOTE: PAD_BEHIND_PROXY=true assumes a TLS-terminating reverse proxy
# (Caddy, nginx, an ingress controller) is in front. If you serve this
# image directly without a proxy, set PAD_TLS_CERT/PAD_TLS_KEY instead;
# otherwise the binary will refuse to start (plaintext-credentials guard).
ENV PAD_MODE=cloud
ENV PAD_HOST=0.0.0.0
ENV PAD_PORT=8080
ENV PAD_STATIC_DIR=/app/frontend/dist
ENV PAD_STORAGE=database
ENV PAD_AUTH_ENABLED=true
ENV PAD_BEHIND_PROXY=true

# Liveness check: hit the unauthenticated /healthz endpoint. Returns 200
# whenever the process is up; does NOT touch the DB (that's /readyz, which
# the orchestrator should probe separately for readiness gating).
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null || exit 1

USER pad:pad

EXPOSE 8080

CMD ["./baki-backend"]
