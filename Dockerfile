# syntax=docker/dockerfile:1

ARG GO_IMAGE=golang:1.27-bookworm
FROM ${GO_IMAGE} AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 go build \
    -buildvcs=false \
    -trimpath \
    -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
    -o /out/cloudfs ./cmd/cloudfs

FROM debian:bookworm-slim

# fuse3 supplies fusermount3 for optional Linux FUSE mounts. MCP-only mode
# needs neither /dev/fuse nor elevated capabilities.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates fuse3 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 65532 cloudfs \
    && useradd --uid 65532 --gid cloudfs --no-create-home --home-dir /nonexistent cloudfs \
    && mkdir -p /config /var/lib/cloudfs \
    && chown cloudfs:cloudfs /config /var/lib/cloudfs

COPY --from=build /out/cloudfs /usr/local/bin/cloudfs

ENV CLOUDFS_CONFIG=/config/config.yaml
VOLUME ["/config", "/var/lib/cloudfs"]
EXPOSE 8080 8765
USER 65532:65532
ENTRYPOINT ["cloudfs"]
CMD ["version"]
