# file-edit-mcp

An MCP (Model Context Protocol) server exposing six file tools — `read`, `write`, `edit`, `multi_edit`, `glob`, `grep` — strictly scoped to the directories given via `--allow`. Mutating tools enforce read-before-write within a session, and edits are rejected when the file changed since it was last read. There are no shell, process, network, clipboard, or screenshot tools. Two transports: stdio (local, one process per client connection) and streamable HTTP (resident server).

## Security model

- **Root scoping.** Only absolute paths under the `--allow` roots are served. Validation is a pipeline: clean/abs normalization, prefix comparison against the allowed roots, then a realpath pass with a second prefix comparison; file operations go through `os.Root` (openat semantics), so `..` traversal and symlink swaps cannot escape a validated root.
- **Read-before-write.** `write`/`edit`/`multi_edit` on an existing file the session has not read are rejected (`EUnreadWrite`); `edit`/`multi_edit` additionally re-check size/mtime at write time and reject stale reads (`EStaleRead`), so a concurrent writer can cause a rejected edit but never a silent overwrite.
- **Atomic writes.** Content lands via a same-directory temp file plus rename; a failed write leaves the original file untouched.
- **No execution surface.** The only subprocess is `rg` behind `grep`, with arguments built by the server from typed fields — no shell, no user-controlled command line.
- **Token-path auth (HTTP).** The MCP endpoint is `/{token}/mcp`; the token comes from `FILE_EDIT_MCP_TOKEN`. Every other path — including wrong tokens — is a plain 404 that never reaches the MCP handler, and the token never appears in logs. A leaked token is full access to the allowed roots: bind the port to loopback or an internal interface only.

## Quick start

### stdio (local)

```sh
go build -o file-edit-mcp ./cmd/file-edit-mcp
./file-edit-mcp --allow /tmp/sandbox
```

Client configuration:

```json
{
  "mcpServers": {
    "file-edit": {
      "command": "/path/to/file-edit-mcp",
      "args": ["--allow", "/tmp/sandbox"]
    }
  }
}
```

### HTTP (resident)

```sh
FILE_EDIT_MCP_TOKEN=secret-token ./file-edit-mcp --transport http --addr 127.0.0.1:8080 --allow /data
```

The MCP endpoint is then `http://127.0.0.1:8080/secret-token/mcp`. Client configuration (URL form):

```json
{
  "mcpServers": {
    "file-edit": { "url": "http://HOST:PORT/TOKEN/mcp" }
  }
}
```

`FILE_EDIT_MCP_TOKEN` is required with `--transport http` and ignored with stdio. The token must match `^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$` (1–128 characters from `[A-Za-z0-9_-]`, leading alphanumeric — URL-path-segment safe and free of routing metacharacters); a missing or invalid token fails at startup, before any route is built. Rotating the token means changing the variable and recreating the process/container.

## Docker

```sh
docker pull myl7/file-edit-mcp:v0.2.1
```

Compose example:

```yaml
services:
  file-edit-mcp:
    image: myl7/file-edit-mcp:v0.2.1
    restart: always
    environment:
      - FILE_EDIT_MCP_TOKEN=change-me   # endpoint becomes /change-me/mcp
      - FILE_EDIT_MCP_INSTRUCTIONS=A notes vault; files are UTF-8 markdown.   # optional, see below
    ports:
      - "127.0.0.1:8080:8080"
    volumes:
      - /data:/data
```

The image entrypoint is the binary; the default CMD is `--transport http --addr :8080 --allow /data` (compose overrides it as a whole, e.g. with more `--allow` dirs or a different `--addr`).

## FILE_EDIT_MCP_INSTRUCTIONS

The initialize result carries an `instructions` field (MCP's counterpart to an AGENTS.md). By default it is a single sentence naming the actual allowed roots — the one fact the tool schemas cannot know. Per-tool semantics (read-first rules, exact-match requirements, truncation limits, argument names) live in the tool descriptions and are deliberately not repeated there: the instructions text is injected into the model's context at the start of every session, and duplicating schema facts in it costs tokens and drifts as descriptions change. Set `FILE_EDIT_MCP_INSTRUCTIONS` to replace the default verbatim when the deployment needs context about the objects being served (for example: what kind of data the roots hold and any house rules for editing it).

## Network filesystem notes

The server is designed to sit in front of CIFS/automounter-mounted shares and stays correct when the mount idles out or drops:

- **Automount idle-unmount.** An unmounted share makes `stat` return `ENOENT`, which would misreport "file does not exist" for a share that is merely not mounted. In HTTP mode, `--keepalive` (default 4m; `0` disables) stats each allowed directory periodically so an automounter idle timeout never fires while the server runs.
- **Backend errors.** `EIO`/`ETIMEDOUT`/`ESTALE`-class errors are retried — with the `os.Root` pool reopened to re-trigger the automount — before surfacing as one distinguishable error; `ENOENT` is never retried.
- **Bind propagation.** If you bind-mount the mount point itself (`/data:/data`, where `/data` is the automounted share), use the compose long syntax with `bind.propagation: rslave` so host unmount/remount events propagate into the container. A subtree bind (a directory below the mount point) cannot receive those events regardless of the propagation setting — recover a stale subtree bind by restarting the container or remounting on the host.

## Development

```sh
make test    # go test ./...
make vet     # go vet ./...
make release VERSION=v0.2.1   # cross-compile, build the scratch image, push
```

Go 1.27, no cgo. `Dockerfile` is the canonical multi-stage build (golang:1.27-alpine build, alpine:3.20 runtime with ripgrep); `Dockerfile.prebuilt` ships the prebuilt static binary from `make linux` in a scratch image (~20 MB). Detailed specification: [ARCHITECTURE.md](ARCHITECTURE.md).

## License

Apache-2.0.
