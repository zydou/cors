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
docker build -t cors-proxy .
docker run -d -p 3000:3000 -e HTTP_PROXY=http://127.0.0.1:7890 cors-proxy
```

The Dockerfile uses multi-stage builds targeting multiple architectures (amd64, arm64, arm, ppc64le, s390x) with `gcr.io/distroless/static-debian12:nonroot` as the base image.

## CI/CD

GitHub Actions workflow (`.github/workflows/build.yml`) triggers on changes to `main.go`, `Dockerfile`, or the workflow file itself. It cross-compiles to all target architectures and pushes a multi-arch image to GHCR.

## Architecture

The entire application lives in `main.go` (~235 lines):

- **`client`** — package-level singleton `http.Client` initialized once at startup. Connection pooling only works because the same client is reused across all requests.
- **`bufPool`** — `sync.Pool` recycling 128KB copy buffers to reduce GC pressure under high concurrency.
- **`proxyHandler`** — single HTTP handler registered on `/`. Extracts the target URL from the request path, validates it, then streams the response back to the client.
- **`createHTTPClient`** — builds the transport with proxy support, tuned connection pool, TCP buffer sizing, and keep-alive. Called once at package init.
- **`fixUrl`** — normalizes URLs missing a protocol prefix.

### Request Flow

`GET /{target-url}` → strip `/` prefix → URL-decode → validate → fix protocol → proxy via global `client` → write headers → `Flush()` → stream response using pooled 128KB buffer.

### Key Design Decisions

- **GET only** — explicitly rejects other methods with a 405 JSON response.
- **No overall timeout** — `Timeout: 0` on the HTTP client to support very large file downloads. Only `TLSHandshakeTimeout` (10s) and `ExpectContinueTimeout` (1s) are set.
- **Global client singleton** — connection pooling requires reusing one `http.Client`; creating one per request would defeat the pool.
- **128KB copy buffer** — used in `io.CopyBuffer` for efficient streaming, pooled via `sync.Pool`.
- **128KB TCP read/write buffers** — set on `http.Transport` to reduce syscall overhead.
- **Flush after WriteHeader** — lowers time-to-first-byte so clients start receiving immediately.
- **Standard `http.HandleFunc`** — no external router; the catch-all handler does its own path parsing.
- **No `go.mod`** — the project uses only the standard library, so no module file is needed.

## Environment Variables

| Variable | Default | Purpose |
|----------|---------|---------|
| `PORT` | `3000` | Server listen port |
| `HTTP_PROXY` | (none) | Upstream HTTP proxy for outbound requests |
