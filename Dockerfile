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
RUN apk add --no-cache ca-certificates libc6-compat

WORKDIR /app
COPY --from=backend-builder /app/baki-backend .
COPY --from=frontend-builder /app/frontend/dist ./frontend/dist

# Default environment variables for cloud-style deployment
ENV PAD_MODE=cloud
ENV PAD_HOST=0.0.0.0
ENV PAD_PORT=8080
ENV PAD_STATIC_DIR=/app/frontend/dist
ENV PAD_STORAGE=database
ENV PAD_AUTH_ENABLED=true

EXPOSE 8080

CMD ["./baki-backend"]
