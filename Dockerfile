# Stage 1: Build the frontend
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
ARG VITE_API_URL
ENV VITE_API_URL=${VITE_API_URL}
RUN npm run build

# Stage 2: Build the backend
FROM golang:1.25-alpine@sha256:523c3effe300580ed375e43f43b1c9b091b68e935a7c3a92bfcc4e7ed55b18c2 AS backend-builder
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
    -ldflags="-s -w -X main.GitCommit=${GIT_COMMIT} -X main.Version=${VERSION} -X pad-analyzer/internal/service.Version=${VERSION}" \
    -o baki-backend main.go

# Stage 3: Final lean image — distroless (no shell/package-manager) for a
# smaller attack surface. The Go binary is CGO_ENABLED=0 (static), so the
# distroless static-debian12 variant is directly compatible. :nonroot ships
# uid 65532, consistent with the ACA securityContext runAsNonRoot in
# infra/main.bicep. Pinned by digest (Dependabot's docker ecosystem keeps it
# current) — a floating tag would regress the pinning discipline applied to the
# other base images.
FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

WORKDIR /app
COPY --from=backend-builder --chown=nonroot:nonroot /app/baki-backend .
COPY --from=frontend-builder --chown=nonroot:nonroot /app/frontend/dist ./frontend/dist

ENV PAD_MODE=cloud
ENV PAD_HOST=0.0.0.0
ENV PAD_PORT=8080
ENV PAD_STATIC_DIR=/app/frontend/dist
ENV PAD_STORAGE=database
ENV PAD_AUTH_ENABLED=true
ENV PAD_BEHIND_PROXY=true

# No Dockerfile HEALTHCHECK: there is no shell/wget in distroless, and under the
# prod target (Azure Container Apps) liveness/readiness are served by the
# platform httpGet probes in infra/main.bicep (/healthz, /readyz). docker-
# compose.{yml,prod.yml} carry their own healthcheck. A bare `docker run` loses
# the in-image liveness signal — operators using it should add --health-cmd.

USER nonroot:nonroot

EXPOSE 8080

CMD ["./baki-backend"]
