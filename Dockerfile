# Stage 1: Build the frontend
FROM node:20-alpine AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
ARG VITE_API_URL
ENV VITE_API_URL=${VITE_API_URL}
RUN npm run build

# Stage 2: Build the backend
FROM golang:1.26-alpine AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
COPY core/go.mod core/go.sum ./core/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
COPY . .
RUN go install github.com/swaggo/swag/cmd/swag@v1.16.6
RUN swag init -g main.go --parseDependency --parseInternal
ARG GIT_COMMIT=dev
ARG VERSION=0.1.0
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.GitCommit=${GIT_COMMIT} -X main.Version=${VERSION}" \
    -o baki-backend main.go

# Stage 3: Final lean image
FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget

RUN addgroup -g 1000 -S pad && adduser -u 1000 -S pad -G pad \
    && mkdir -p /home/pad \
    && chown pad:pad /home/pad

WORKDIR /app
COPY --from=backend-builder --chown=pad:pad /app/baki-backend .
COPY --from=frontend-builder --chown=pad:pad /app/frontend/dist ./frontend/dist

ENV PAD_MODE=cloud
ENV PAD_HOST=0.0.0.0
ENV PAD_PORT=8080
ENV PAD_STATIC_DIR=/app/frontend/dist
ENV PAD_STORAGE=database
ENV PAD_AUTH_ENABLED=true
ENV PAD_BEHIND_PROXY=true

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- "http://localhost:${PAD_PORT}/healthz" >/dev/null || exit 1

USER pad:pad

EXPOSE 8080

CMD ["./baki-backend"]
