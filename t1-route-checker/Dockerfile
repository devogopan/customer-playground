FROM --platform=$BUILDPLATFORM golang:1.25 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION}" -o /validate-routes .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /validate-routes /validate-routes
EXPOSE 8000
ENTRYPOINT ["/validate-routes"]
