# Multi-arch build for ddgo-mcp.
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t <registry>/ddgo-mcp:0.1.0 --push .
FROM --platform=$BUILDPLATFORM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS TARGETARCH VERSION=dev
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/ddgo-mcp .

# distroless/static ships CA certificates (needed for HTTPS search/fetch) and
# runs as nonroot — a few MB, no shell, no Python, no browser.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/ddgo-mcp /ddgo-mcp
ENV ADDR=:8000
EXPOSE 8000
USER nonroot:nonroot
ENTRYPOINT ["/ddgo-mcp"]
