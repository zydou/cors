# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A minimal HTTP proxy server written in Go for downloading overseas resources from domestic servers. Single-file application (`main.go`) with no external dependencies — uses only Go standard library.

## Build & Run

```bash
# Build for current platform
go build -o cors main.go

# Run (default port 3000)
./cors

# Run with custom port and upstream proxy
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 ./cors

# Cross-compile for Linux (used by Dockerfile)
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cors-linux-amd64 main.go
```

## Docker

```bash
docker build -t cors .
docker run -d -p 3000:3000 -e HTTP_PROXY=http://127.0.0.1:7890 cors
```

The Dockerfile uses multi-stage builds targeting multiple architectures (amd64, arm64, arm, ppc64le, s390x) with `gcr.io/distroless/static-debian12:nonroot` as the base image.

## CI/CD

GitHub Actions workflow (`.github/workflows/build.yml`) triggers on changes to `main.go`, `Dockerfile`, or the workflow file itself. It cross-compiles to all target architectures and pushes a multi-arch image to GHCR.

## Architecture

The entire application lives in `main.go`:

- **`client`** — package-level singleton `http.Client` initialized once at startup. Connection pooling only works because the same client is reused across all requests.
- **`bufPool`** — `sync.Pool` recycling 512KB copy buffers to reduce GC pressure under high concurrency.
- **`proxyHandler`** — the server's single HTTP handler (no `ServeMux`, which would rewrite `//` in the target URL with a 307). Extracts the target URL from the request path, validates it, then streams the upstream response to the client over a single connection.
- **`createHTTPClient`** — builds the transport with proxy support, tuned connection pool, TCP buffer sizing, and kernel-level socket buffer control. Called once at package init.
- **`forwardHeaders`** — forwards client headers to upstream, removing `Referer` (bypass hotlink protection), `Host`, and hop-by-hop headers. `Range` is forwarded so resumable downloads work end to end.
- **`flushWriter`** — wraps `io.Writer` and flushes immediately after each successful write. Flush MUST happen on the request goroutine: `http.ResponseWriter`'s underlying `bufio.Writer` is not thread-safe, and flushing from a background goroutine races with `Write` (causes `io.ErrShortWrite` and truncated responses).
- **`setCORSHeaders`** — sets `Access-Control-Allow-Origin: *` and related headers for browser-based clients.
- **`fixUrl`** — normalizes URLs missing a protocol prefix.
- **`copyHeaders`** — copies upstream response headers verbatim (including `Content-Length`, `Content-Range`, `Accept-Ranges`) so clients see real file sizes and can resume.

### Request Flow

```
GET /{target-url}
  → strip `/` prefix (r.URL.Path is already percent-decoded by net/http — do NOT
    run url.QueryUnescape on it: it would turn `+` into a space and corrupt
    filenames like cpython-3.11.16+20260825)
  → fix protocol → append raw query string → validate (http/https scheme and non-empty host, else 400)
  → forward client headers (Range included) → GET upstream
  → copy upstream status + headers → stream body via io.CopyBuffer + inline flush
```

### Key Design Decisions

- **CORS enabled** — all responses include `Access-Control-Allow-Origin: *`, allowing browser-based clients to use the proxy directly. OPTIONS preflight returns 204.
- **Transparent streaming proxy** — upstream status, headers (including `Content-Length`/`Content-Range`/`Accept-Ranges`), and client `Range` requests pass through unchanged. Resumable downloads (`curl -C -`) work without the proxy tracking any state; a mid-transfer disconnect is the client's to retry with `Range`.
- **Client header forwarding** — all client headers (User-Agent, Accept, Accept-Language, Cookie, etc.) are forwarded to upstream. `Referer` is stripped to bypass hotlink protection; `Host` and hop-by-hop headers are managed by the proxy.
- **Trusted users, isolated container** — the proxy is meant for trusted users and runs in an isolated distroless container, so there is no SSRF protection: any host, including loopback and private addresses, can be reached by design. Do not re-add IP filtering.
- **GET only** — explicitly rejects other methods with a 405 JSON response.
- **No overall timeout** — the `http.Client` sets no `Timeout`, so very large file downloads are not cut off. Only `TLSHandshakeTimeout` (10s) is set on the transport.
- **Global client singleton** — connection pooling requires reusing one `http.Client`; creating one per request would defeat the pool.
- **512KB copy buffer** — used in `io.CopyBuffer` for efficient streaming, pooled via `sync.Pool`. Aligns with `ReadBufferSize`/`WriteBufferSize` to minimize syscalls.
- **2MB kernel socket buffer** — set via `net.Dialer.Control` (`setsockopt(SO_RCVBUF/SO_SNDBUF)`), separate from Go's user-sized `ReadBufferSize`/`WriteBufferSize` which only affects `bufio`. Essential for high-BDP cross-border links.
- **Inline flush** — `flushWriter` flushes after every write, so data read from the (possibly slow) upstream is pushed to the client immediately instead of sitting in the server's bufio buffers.

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `3000` | Server listen port |
| `HTTP_PROXY` | (none) | Upstream HTTP proxy for outbound requests |
| `GITHUB_TOKEN` | (none) | Auto-injected as `Authorization` on GitHub downloads when the client provides none |
