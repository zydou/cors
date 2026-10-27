package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

// githubToken 服务端统一配置的 GitHub Token，客户端未提供 Authorization 时自动注入
var githubToken = os.Getenv("GITHUB_TOKEN")

// Response 统一的 JSON 响应格式
type Response struct {
	Code  int    `json:"code"`
	Usage string `json:"usage"`
	Note  string `json:"note"`
	Msg   string `json:"msg,omitempty"`
}

const (
	usage = "GET Host/{URL}"
	note  = "Proxy server for downloading files from external resources"
)

// 全局复用 HTTP 客户端，连接池才能生效
var client = createHTTPClient()

// copy buffer 池化，减少 GC 压力
var bufPool = sync.Pool{
	New: func() any { return make([]byte, 512*1024) },
}

// createHTTPClient 创建支持代理的 HTTP 客户端（全局只调用一次）
func createHTTPClient() *http.Client {
	transport := &http.Transport{
		// 连接池调优
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,

		// 用户态 bufio 缓冲区，与 copy buffer 对齐以减少 syscall 次数
		ReadBufferSize:  512 * 1024,
		WriteBufferSize: 512 * 1024,

		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			// ReadBufferSize 只影响用户态，高 BDP 跨境链路（如 100Mbps × 200ms）
			// 必须显式设置内核 socket 缓冲区为 2MB
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2<<20)
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 2<<20)
				})
			},
		}).DialContext,

		TLSHandshakeTimeout: 10 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	if httpProxy := os.Getenv("HTTP_PROXY"); httpProxy != "" {
		proxyURL, err := url.Parse(httpProxy)
		if err != nil {
			log.Printf("Failed to parse proxy URL: %v, using direct connection", err)
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}

	// 不设置 Timeout，大文件下载不能有整体超时；重定向沿用 net/http 默认上限
	return &http.Client{Transport: transport}
}

// flushWriter 每次写入成功后立即 Flush，让慢速上游的数据及时推送给客户端。
// 必须在处理请求的同一 goroutine 内 Flush：http.ResponseWriter 底层的 bufio.Writer
// 不是线程安全的，跨 goroutine Flush 会与 Write 竞争，导致响应提前截断。
type flushWriter struct {
	w  io.Writer
	rc *http.ResponseController
}

func (fw flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil {
		err = fw.rc.Flush()
	}
	return n, err
}

// copyHeaders 复制上游响应头到客户端（原样透传，含 Content-Length /
// Content-Range / Accept-Ranges，保证客户端可见文件大小并可断点续传；
// hop-by-hop 头已由 net/http 剥离）
func copyHeaders(w http.ResponseWriter, resp *http.Response) {
	for name, values := range resp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}
}

// fixUrl 补全协议前缀，兼容被合并斜杠的 "https:/host/..." 形式
func fixUrl(u string) string {
	if strings.Contains(u, "://") {
		return u
	}
	if strings.Contains(u, ":/") {
		return strings.Replace(u, ":/", "://", 1)
	}
	return "http://" + u
}

// isGitHubURL 判断目标是否为 GitHub 相关域名（github.com / *.github.com / *.githubusercontent.com）
func isGitHubURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	return host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".githubusercontent.com")
}

// forwardHeaders 将客户端请求头转发给上游。删除 Referer（绕过防盗链）和 hop-by-hop 头；
// GitHub 目标且客户端未带 Authorization 时，注入服务端 GITHUB_TOKEN
func forwardHeaders(dst, src http.Header, u *url.URL) {
	for key, values := range src {
		switch strings.ToLower(key) {
		case "referer", "host",
			"connection", "transfer-encoding", "te", "trailer", "upgrade",
			"proxy-authorization", "proxy-connection":
			continue
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}

	if src.Get("Authorization") == "" && githubToken != "" && isGitHubURL(u) {
		dst.Set("Authorization", "Bearer "+githubToken)
	}
}

// setCORSHeaders 设置跨域响应头，允许浏览器直接访问代理
func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Max-Age", "86400") // 预检缓存 24h
}

// writeJSON 返回统一格式的 JSON 响应
func writeJSON(w http.ResponseWriter, status, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(Response{Code: code, Usage: usage, Note: note, Msg: msg})
}

// proxyHandler 请求路径 /{target-url} 即上游地址，流式转发上游响应
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	setCORSHeaders(w)

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, http.StatusMethodNotAllowed, "Only GET requests are allowed")
		return
	}

	// r.URL.Path 已由 net/http 完成百分号解码，不能再用 url.QueryUnescape：
	// 它会把 + 解成空格，破坏 cpython-3.11.16+20260825 这类文件名
	target := strings.TrimPrefix(r.URL.Path, "/")
	if target == "" {
		writeJSON(w, http.StatusOK, 0, "")
		return
	}

	// 查询串需一并转发（如 CDN 签名参数）。fixUrl 只处理路径部分，
	// 避免查询串里的 "://" 被误判为目标已带协议
	target = fixUrl(target)
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	proxyReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, target, nil)
	if err != nil || (proxyReq.URL.Scheme != "http" && proxyReq.URL.Scheme != "https") || proxyReq.URL.Host == "" {
		writeJSON(w, http.StatusBadRequest, http.StatusBadRequest, "Invalid URL")
		return
	}
	forwardHeaders(proxyReq.Header, r.Header, proxyReq.URL)
	log.Printf("Proxying GET request to: %s", proxyReq.URL)

	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("Error proxying request: %v", err)
		writeJSON(w, http.StatusInternalServerError, -1, err.Error())
		return
	}
	defer proxyResp.Body.Close()

	copyHeaders(w, proxyResp)
	w.WriteHeader(proxyResp.StatusCode)

	buf := bufPool.Get().([]byte)
	defer bufPool.Put(buf)

	// 响应头和部分 body 已发出，无法再返回 JSON 错误，只能记录日志，客户端会收到截断的响应
	n, err := io.CopyBuffer(flushWriter{w: w, rc: http.NewResponseController(w)}, proxyResp.Body, buf)
	if err != nil {
		log.Printf("Error copying response body to client after %d bytes: %v (target: %s)", n, err, proxyReq.URL)
		return
	}

	// 记录上游状态码：4xx/5xx 时字节数很小，仅凭字节数无法区分成功与错误页
	log.Printf("Proxied %d bytes (upstream status %d) from: %s", n, proxyResp.StatusCode, proxyReq.URL)
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	if httpProxy := os.Getenv("HTTP_PROXY"); httpProxy != "" {
		log.Printf("HTTP_PROXY configured: %s", httpProxy)
	} else {
		log.Println("HTTP_PROXY not configured, using direct connection")
	}
	if githubToken != "" {
		log.Println("GITHUB_TOKEN configured: GitHub downloads will use server token for higher rate limits")
	} else {
		log.Println("GITHUB_TOKEN not configured: GitHub downloads will be unauthenticated (60 req/h limit)")
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "3000"
	}
	srv := &http.Server{Addr: "0.0.0.0:" + port, Handler: http.HandlerFunc(proxyHandler)}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("Proxy server is running on http://%s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-ctx.Done()
	stop()
	log.Println("Shutting down gracefully...")

	// ListenAndServe 在 Shutdown 开始时就会返回，因此这里必须阻塞等待 Shutdown 结束，
	// 否则 main 直接退出会中断进行中的下载。最多等待 30 秒
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
	}
	log.Println("Server stopped.")
}
