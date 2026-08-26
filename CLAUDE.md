# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

A minimal HTTP proxy server written in Go for downloading overseas resources from domestic servers. Single-file application (`main.go`) with no external dependencies — uses only Go standard library.

## Build & Run

```bash
# Build for current platform
go build -o cors main.go

# Run (default port 3000, 8 parallel connections)
./cors

# Run with custom port, upstream proxy, and parallel connections
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 PARALLEL_CONNS=8 ./cors

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
- **`parallelConns`** — configurable number of parallel connections for Range downloading (default 8, range 1–16, override via `PARALLEL_CONNS` env var).
- **`proxyHandler`** — single HTTP handler registered on `/`. Extracts the target URL from the request path, validates it, then either uses parallel Range download (large files) or single-connection streaming (small files).
- **`createHTTPClient`** — builds the transport with proxy support, tuned connection pool, TCP buffer sizing, and kernel-level socket buffer control. Called once at package init.
- **`probeRangeSupport`** — sends HEAD request to check if target supports Range requests and get file size.
- **`parallelDownload`** — splits file into 2MB chunks, downloads in parallel using semaphore-goroutine pipeline, writes chunks in order to client.
- **`downloadChunk`** — downloads a single byte range and validates returned size matches request.
- **`forwardHeaders`** — forwards client headers to upstream, removing `Referer` (bypass hotlink protection), `Host`, `Range`, and hop-by-hop headers.
- **`flushWriter`** — wraps `io.Writer` with periodic flushing (100ms) to reduce client-perceived latency.
- **`setCORSHeaders`** — sets `Access-Control-Allow-Origin: *` and related headers for browser-based clients.
- **`fixUrl`** — normalizes URLs missing a protocol prefix.
- **`copyHeaders`** — copies response headers, filtering `Content-Length` and `Accept-Ranges` to avoid conflicts.

### Request Flow

```
GET /{target-url}
  → strip `/` prefix (r.URL.Path is already percent-decoded by net/http — do NOT
    run url.QueryUnescape on it: it would turn `+` into a space and corrupt
    filenames like cpython-3.11.16+20260825)
  → validate → fix protocol
  → probeRangeSupport (HEAD, 10s timeout)
      ├── Supports Range + ≥ 2MB → parallelDownload
      │     → split into 2MB chunks
      │     → download up to 4 chunks concurrently (semaphore + ctx.Done())
      │     → write chunks in order, flush after each
      └── Fallback → single-connection streaming (io.CopyBuffer)
```

### Parallel Download Logic

For files ≥ 2MB from servers that support `Accept-Ranges: bytes`:

1. HEAD probe to get `Content-Length` and check `Accept-Ranges`/`Content-Encoding`
2. Split into fixed 2MB chunks (last chunk may be smaller)
3. Spawn goroutines lazily: only when semaphore slot is available
4. Each goroutine downloads its range via `GET` with `Range` header
5. Validates `len(data) == end - start + 1` (catches servers ignoring Range)
6. Main goroutine receives chunks via per-chunk buffered channels, writes in order
7. Client disconnect (`r.Context()`) cancels all in-flight downloads

Fallback to single-connection when:
- Server doesn't advertise `Accept-Ranges: bytes`
- No `Content-Length` (chunked transfer)
- Response has `Content-Encoding` (gzip/br — would corrupt on reassembly)
- File < 2MB

### Key Design Decisions

- **CORS enabled** — all responses include `Access-Control-Allow-Origin: *`, allowing browser-based clients to use the proxy directly. OPTIONS preflight returns 204.
- **Client header forwarding** — all client headers (User-Agent, Accept, Accept-Language, Cookie, etc.) are forwarded to upstream. `Referer` is stripped to bypass hotlink protection; `Host`, `Range`, and hop-by-hop headers are managed by the proxy.
- **GET only** — explicitly rejects other methods with a 405 JSON response.
- **No overall timeout** — `Timeout: 0` on the HTTP client to support very large file downloads. Only `TLSHandshakeTimeout` (10s) and `ExpectContinueTimeout` (1s) are set. Probe uses a separate 10s timeout.
- **Global client singleton** — connection pooling requires reusing one `http.Client`; creating one per request would defeat the pool.
- **512KB copy buffer** — used in `io.CopyBuffer` for efficient streaming, pooled via `sync.Pool`. Aligns with `ReadBufferSize`/`WriteBufferSize` to minimize syscalls.
- **2MB kernel socket buffer** — set via `net.Dialer.Control` (`setsockopt(SO_RCVBUF/SO_SNDBUF)`), separate from Go's user-sized `ReadBufferSize`/`WriteBufferSize` which only affects `bufio`. Essential for high-BDP cross-border links.
- **Periodic flush** — `flushWriter` ticks every 100ms to push data to clients, preventing buffering delays on slow upstream connections.
- **Semaphore-goroutine pipeline** — fixed 2MB chunkSize with lazy goroutine spawning: only `parallelConns` goroutines exist at any time, memory bounded to `parallelConns × chunkSize`.
- **zero `go.mod`** — the project uses only the standard library, so no module file is needed.

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `3000` | Server listen port |
| `HTTP_PROXY` | (none) | Upstream HTTP proxy for outbound requests |
| `PARALLEL_CONNS` | `8` | Parallel connections for large file downloads (1–16, set to 1 to disable) |
