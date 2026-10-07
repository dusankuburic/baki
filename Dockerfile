# Stage 1: Build the frontend
FROM node:20-alpine@sha256:fb4cd12c85ee03686f6af5362a0b0d56d50c58a04632e6c0fb8363f609372293 AS frontend-builder
WORKDIR /app/frontend
COPY frontend/package*.json ./
RUN --mount=type=cache,target=/root/.npm npm ci
COPY frontend/ ./
ARG VITE_API_URL
ENV VITE_API_URL=${VITE_API_URL}
RUN npm run build
# Ship the bundle without its source maps. vite's sourcemap:'hidden' only drops
# the //# sourceMappingURL comment; the .map files are still written to dist/,
# they carry sourcesContent (the complete original TypeScript), and the final
# stage serves every file under the static dir — so shipping dist/ verbatim
# published the app's source at a guessable URL. They were also ~9.6 MB of a
# 14 MB dist, for a build nothing symbolicates.
#
# Stripped into a separate directory rather than deleted in place, so
# `--target frontend-builder` still has the maps for anyone who wants to
# upload them to an error tracker.
RUN cp -a dist dist-nomaps && find dist-nomaps -name '*.map' -delete
# Precompress the shipped bundle once (brotli max quality + gzip -9, .br/.gz
# next to each file); the server serves those siblings instead of compressing
# every asset on every request (26 ms of CPU for the main chunk with brotli).
# Run on the map-stripped copy, after the strip, so no map gets a sibling.
RUN node scripts/precompress.mjs dist-nomaps
# The bundle's build version, the same VERSION the backend stage stamps: the
# app offers a reload when the server's X-Baki-Version differs (a tab left open
# across a deploy). Written into index.html's baki-version meta last, not into
# the JS: VERSION changes on every commit, and in a chunk it would rename every
# chunk and miss the build and precompress layers on a backend-only deploy
# (index.html is not precompressed). Same default as the backend's, so an
# unstamped build matches itself.
ARG VERSION=0.1.0
RUN sed -i "s|__BAKI_VERSION__|${VERSION}|" dist-nomaps/index.html

# Stage 2: Build the backend
FROM golang:1.25-alpine@sha256:523c3effe300580ed375e43f43b1c9b091b68e935a7c3a92bfcc4e7ed55b18c2 AS backend-builder
WORKDIR /app
COPY go.mod go.sum ./
COPY core/go.mod core/go.sum ./core/
RUN --mount=type=cache,target=/go/pkg/mod go mod download
# Before COPY . . so a source change does not re-download and re-link swag;
# the module cache mount means its dependencies are fetched once per builder.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    go install github.com/swaggo/swag/cmd/swag@v1.16.6
COPY . .
# Same invocation as the OpenAPI Freshness CI job, so the spec baked into the
# image is the one CI validated. (Verified identical output to the previous
# --parseDependency --parseInternal form, but identical-by-accident is not a
# property worth relying on.)
RUN --mount=type=cache,target=/go/pkg/mod \
    swag init -g main.go -o docs --parseDependencyLevel 1
ARG GIT_COMMIT=dev
ARG VERSION=0.1.0
# Commit date, not build time: keeps the image reproducible from a given commit
# while still answering "how old is what's running".
ARG BUILD_DATE=
# /api/system/info reports Version, GitCommit and BuildDate — and it reads them
# from internal/service, NOT from main. Only internal/service.Version was being
# stamped, so the endpoint operators use to identify a deployment always
# answered gitCommit="" and buildDate="". main.Version is stamped too because
# telemetry.Init reports it as the OTel service version; main.GitCommit is
# stamped for symmetry, though nothing reads it today.
# Both caches are BuildKit mounts, not layers: the build cache turns a
# one-file change into an incremental compile instead of a full rebuild of
# every package, and the module mount stops the build re-fetching what
# `go mod download` already put in the (equally non-layer) module cache.
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.Version=${VERSION} -X main.GitCommit=${GIT_COMMIT} -X pad-analyzer/internal/service.Version=${VERSION} -X pad-analyzer/internal/service.GitCommit=${GIT_COMMIT} -X pad-analyzer/internal/service.BuildDate=${BUILD_DATE}" \
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
COPY --from=frontend-builder --chown=nonroot:nonroot /app/frontend/dist-nomaps ./frontend/dist

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
