# Canonical multi-stage image (ARCHITECTURE.md §0/§10).
# Build on a machine with Docker + network and ~2 GB free RAM for Go compile.
# Do NOT build this on memory-starved machines — use Dockerfile.prebuilt.
#
#   docker build -t file-edit-mcp:latest .

FROM golang:1.27-alpine AS build
WORKDIR /src

# Cache module downloads: manifests first, then source.
COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" \
    -o /out/file-edit-mcp ./cmd/file-edit-mcp

FROM alpine:3.20
# ripgrep backs the grep tool (ARCHITECTURE.md §5.6).
RUN apk add --no-cache ripgrep

COPY --from=build /out/file-edit-mcp /usr/local/bin/file-edit-mcp
RUN chmod 0755 /usr/local/bin/file-edit-mcp

# Resident HTTP server form (ARCHITECTURE.md §0/§10): the binary is the
# entrypoint, serving streamable HTTP on :8080 with the MCP endpoint at
# /{token}/mcp. Compose files override CMD as a whole (e.g. more --allow
# dirs or a different --addr). Auth is token-path: the container refuses to
# start without FILE_EDIT_MCP_TOKEN set in its environment (1-128 chars of
# [A-Za-z0-9_-], leading alphanumeric) — knowing the URL IS knowing the
# credential, so rotate by changing the env and recreating the container;
# a leaked token is full access, so still bind the published port to
# loopback or an internal interface only. When an allowed root is an
# automounted share bind-mounted at its mount point, use rslave
# propagation so host remounts propagate into the container.
ENTRYPOINT ["/usr/local/bin/file-edit-mcp"]
CMD ["--transport", "http", "--addr", ":8080", "--allow", "/data"]
EXPOSE 8080
