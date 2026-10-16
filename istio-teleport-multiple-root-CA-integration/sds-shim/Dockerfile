FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY main.go ./
ARG TARGETOS=linux TARGETARCH=amd64
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /sds-shim .

# No shell, no package manager, no libc: just the static binary.
FROM scratch
COPY --from=build /sds-shim /sds-shim
USER 65532:65532
ENTRYPOINT ["/sds-shim"]
