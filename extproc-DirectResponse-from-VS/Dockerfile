# syntax=docker/dockerfile:1

# ----- Builder -----
FROM golang:1.23 AS builder
WORKDIR /app

# Cache deps first
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source
COPY . .

# Build static binary
RUN --mount=type=cache,target=/go/pkg/mod \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 \
    go build -o /extproc-server ./main.go

# ----- Runner -----
FROM gcr.io/distroless/static-debian12
WORKDIR /
COPY --from=builder /extproc-server /extproc-server
EXPOSE 9000
USER nonroot:nonroot
ENTRYPOINT ["/extproc-server"]
