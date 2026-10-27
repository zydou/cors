package main

import (
	"context"
	"encoding/json"
	"fmt"
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

// isPrivateIP 检查 IP 是否为私有/保留/链路本地地址，用于 SSRF 防护
func isPrivateIP(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}
	// 覆盖所有私有 CIDR
	privateBlocks := []string{
		"10.0.0.0/8",     // RFC1918
		"172.16.0.0/12",  // RFC1918
		"192.168.0.0/16", // RFC1918
		"127.0.0.0/8",    // Loopback
		"169.254.0.0/16", // Link-local / 云元数据
		"100.64.0.0/10",  // CGNAT (RFC6598)
		"::1/128",        // IPv6 loopback
		"fc00::/7",       // IPv6 unique local
		"fe80::/10",      // IPv6 link-local
	}
	for _, cidr := range privateBlocks {
		_, block, err := net.ParseCIDR(cidr)
		if err == nil && block.Contains(ip) {
			return true
		}
	}
	return false
}

// validateTargetURL 验证目标 URL 是否允许访问（SSRF 防护）
// 解析 hostname 为任一 IP 落在私有范围则拒绝
func validateTargetURL(rawURL string) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("invalid URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("URL has no host")
	}

	// 先检查 host 本身是不是字面 IP（避免 DNS 重绑定：解析后再查一次）
	if ip := net.ParseIP(host); ip != nil {
		if isPrivateIP(ip) {
			return fmt.Errorf("access to private/internal IP %s is forbidden (SSRF protection)", host)
		}
		return nil
	}

	// 域名：解析后检查所有 A/AAAA 记录
	ips, err := net.LookupIP(host)
	if err != nil {
		// 无法解析的域名直接拒绝，不继续请求（安全侧）
		return fmt.Errorf("cannot resolve host %s: %w", host, err)
	}
	for _, ip := range ips {
		if isPrivateIP(ip) {
			return fmt.Errorf("host %s resolves to private/internal IP %s (SSRF protection)", host, ip)
		}
	}
	return nil
}

// githubToken 服务端统一配置的 GitHub Token，用于提高 GitHub 资源下载的限流额度
// 客户端未提供 Authorization 时自动注入
var githubToken = os.Getenv("GITHUB_TOKEN")

// isGitHubURL 判断目标 URL 是否为 GitHub 相关域名（github.com / *.github.com / *.githubusercontent.com）
func isGitHubURL(u *url.URL) bool {
	host := strings.ToLower(u.Hostname())
	if host == "github.com" || strings.HasSuffix(host, ".github.com") || strings.HasSuffix(host, ".githubusercontent.com") {
		return true
	}
	return false
}

// Response 标准响应格式
type Response struct {
	Code  int    `json:"code"`
	Usage string `json:"usage"`
	Note  string `json:"note"`
	Msg   string `json:"msg,omitempty"`
}

// flushWriter 包装 io.Writer，每次写入成功后立即 Flush，确保数据及时推送
// 必须在处理请求的同一 goroutine 内 Flush：http.ResponseWriter 底层的
// bufio.Writer 不是线程安全的，从其他 goroutine 并发 Flush 会产生数据竞争，
// 破坏缓冲区状态导致 Write 返回 short write、响应流提前中断。
// （此前用后台 ticker goroutine 周期性 Flush 正是该竞态的根因）
type flushWriter struct {
	w       io.Writer
	flusher http.Flusher
}

func newFlushWriter(w io.Writer, flusher http.Flusher) *flushWriter {
	return &flushWriter{w: w, flusher: flusher}
}

func (fw *flushWriter) Write(p []byte) (n int, err error) {
	n, err = fw.w.Write(p)
	if err == nil && fw.flusher != nil {
		fw.flusher.Flush()
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

// fixUrl 修复 URL 协议前缀
func fixUrl(u string) string {
	if strings.Contains(u, "://") {
		return u
	} else if strings.Contains(u, ":/") {
		return strings.Replace(u, ":/", "://", 1)
	} else {
		return "http://" + u
	}
}

// forwardHeaders 将客户端请求头转发给上游，删除不应传递的头
// 删除 Referer（绕过防盗链）、Host（Go 自动设置为目标地址）
// 对 GitHub 目标，若客户端未提供 Authorization，自动注入服务端 GITHUB_TOKEN
func forwardHeaders(dst, src http.Header, targetURL string) {
	clientHasAuth := false
	for key, values := range src {
		// 跳过代理自身需要设置的头和可能引发问题的头
		switch strings.ToLower(key) {
		case "referer", "host",
			"connection", "transfer-encoding", "te", "trailer", "upgrade",
			"proxy-authorization", "proxy-connection":
			continue
		}
		if strings.EqualFold(key, "Authorization") {
			clientHasAuth = true
		}
		for _, v := range values {
			dst.Add(key, v)
		}
	}

	// 服务端统一 Token 注入：GitHub 目标 + 客户端未自带认证 + 服务端配了 Token
	if !clientHasAuth && githubToken != "" {
		if u, err := url.Parse(targetURL); err == nil && isGitHubURL(u) {
			dst.Set("Authorization", "Bearer "+githubToken)
		}
	}
}

// 全局复用 HTTP 客户端 —— 连接池真正生效
var client = createHTTPClient()

// copy buffer 池化，减少 GC 压力
var bufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 512*1024)
	},
}

// createHTTPClient 创建支持代理的 HTTP 客户端（全局只调用一次）
func createHTTPClient() *http.Client {
	httpProxy := os.Getenv("HTTP_PROXY")

	transport := &http.Transport{
		// 连接池调优：提高每主机空闲连接上限
		MaxIdleConns:        1000,
		MaxIdleConnsPerHost: 100,
		IdleConnTimeout:     90 * time.Second,

		// 用户态 bufio 缓冲区（不影响内核 socket buffer）
		// 与 copy buffer 大小对齐，减少 syscall 次数
		ReadBufferSize:  512 * 1024,
		WriteBufferSize: 512 * 1024,

		// 自定义拨号器：启用 TCP keep-alive，及时检测死连接
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
			// 设置内核级 socket 缓冲区（2MB）
			// ReadBufferSize/WriteBufferSize 只影响用户态 bufio，不影响内核
			// 对于高 BDP 跨境链路（如 100Mbps × 200ms = 2.5MB），必须显式设置
			// 设大不设小：内核 clamp 到最大值，设大了无害
			Control: func(network, address string, c syscall.RawConn) error {
				return c.Control(func(fd uintptr) {
					// 设置内核发送/接收缓冲区为 2MB
					// 设大不设小：内核会 clamp 到 net.core.rmem_max，设大了无害，设小了可能成为瓶颈
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2<<20)
					syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, 2<<20)
				})
			},
		}).DialContext,

		// 超时控制（Timeout: 0 仅限制整体，这里细化各阶段）
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		ResponseHeaderTimeout: 0, // 不限制，适配大文件

		ForceAttemptHTTP2: true,
	}

	if httpProxy != "" {
		proxyURL, err := url.Parse(httpProxy)
		if err != nil {
			log.Printf("Failed to parse proxy URL: %v, using direct connection", err)
		} else {
			transport.Proxy = http.ProxyURL(proxyURL)
			log.Printf("Using proxy: %s", httpProxy)
		}
	} else {
		log.Println("Using direct connection (no proxy configured)")
	}

	return &http.Client{
		Timeout: 0, // 无整体超时限制，适用于大文件下载
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // 自动跟随重定向
		},
	}
}

// proxyHandler 代理请求处理函数
// setCORSHeaders 设置跨域响应头，允许浏览器直接访问代理
func setCORSHeaders(w http.ResponseWriter) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "*")
	w.Header().Set("Access-Control-Max-Age", "86400") // 预检缓存 24h
}

func proxyHandler(w http.ResponseWriter, r *http.Request) {
	// 设置 CORS 头（所有响应都需要）
	setCORSHeaders(w)

	// 处理 OPTIONS 预检请求
	if r.Method == "OPTIONS" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// 只允许 GET 请求
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		res := Response{
			Code:  405,
			Usage: "GET Host/{URL}",
			Note:  "Only GET requests are allowed",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 从 URL 中提取代理目标地址
	// 注意：r.URL.Path 已由 net/http 完成百分号解码，无需再手动 unescape。
	// 不能用 url.QueryUnescape——它按查询串规则把 + 解成空格，
	// 会破坏含 + 的文件名（如 cpython-3.11.16+20260825）
	targetUrl := strings.TrimPrefix(r.URL.Path, "/")
	if len(targetUrl) == 0 {
		w.WriteHeader(http.StatusOK)
		res := Response{
			Code:  0,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 忽略特定请求
	if len(targetUrl) < 3 || !strings.Contains(targetUrl, ".") || targetUrl == "favicon.ico" || targetUrl == "robots.txt" {
		w.WriteHeader(http.StatusBadRequest)
		res := Response{
			Code:  400,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   "Invalid URL",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 修复 URL 协议
	targetUrl = fixUrl(targetUrl)

	// 仅允许 http/https 协议
	u, err := url.Parse(targetUrl)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		w.WriteHeader(http.StatusBadRequest)
		res := Response{
			Code:  400,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   "Invalid URL: only http and https schemes are allowed",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// SSRF 防护：检查解析后的 IP 是否落在私有/保留地址段
	if err := validateTargetURL(targetUrl); err != nil {
		log.Printf("Blocked private/internal request to %s from %s: %v", targetUrl, r.RemoteAddr, err)
		w.WriteHeader(http.StatusForbidden)
		res := Response{
			Code:  403,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   err.Error(),
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	log.Printf("Proxying GET request to: %s", targetUrl)

	// 单连接流式转发：客户端头原样透传（含 Range，支持断点续传），
	// 上游响应头原样透传（含 Content-Length / Accept-Ranges），状态码一致
	proxyReq, err := http.NewRequestWithContext(r.Context(), "GET", targetUrl, nil)
	if err != nil {
		log.Printf("Error creating request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		res := Response{Code: -1, Usage: "GET Host/{URL}", Note: "Proxy server for downloading files from external resources", Msg: err.Error()}
		json.NewEncoder(w).Encode(res)
		return
	}
	forwardHeaders(proxyReq.Header, r.Header, targetUrl)

	proxyResp, err := client.Do(proxyReq)
	if err != nil {
		log.Printf("Error proxying request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		res := Response{
			Code:  -1,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   err.Error(),
		}
		json.NewEncoder(w).Encode(res)
		return
	}
	defer proxyResp.Body.Close()

	// 复制上游响应头（hop-by-hop 头已由 net/http 剥离）
	copyHeaders(w, proxyResp)

	// 写入响应状态和内容
	w.WriteHeader(proxyResp.StatusCode)

	// 从池中获取 buffer，减少分配
	buf := bufPool.Get().([]byte)
	defer bufPool.Put(buf)

	// 包装 writer，每次写入后立即 flush，确保慢源站场景下数据及时推送
	flusher, _ := w.(http.Flusher)
	fw := newFlushWriter(w, flusher)

	n, err := io.CopyBuffer(fw, proxyResp.Body, buf)
	if err != nil {
		// 此时 header 和 body 已部分写入，无法再返回 JSON 错误响应
		// 只能记录日志，客户端会收到截断的响应
		log.Printf("Error copying response body to client after %d bytes: %v (target: %s)", n, err, targetUrl)
		return
	}

	// 记录上游状态码：4xx/5xx 时字节数很小，仅凭字节数无法区分成功与错误页
	log.Printf("Proxied %d bytes (upstream status %d) from: %s", n, proxyResp.StatusCode, targetUrl)
}

func main() {
	// 设置日志格式
	log.SetFlags(log.LstdFlags | log.Lshortfile)

	// 检查并打印代理配置
	httpProxy := os.Getenv("HTTP_PROXY")
	if httpProxy != "" {
		log.Printf("HTTP_PROXY configured: %s", httpProxy)
	} else {
		log.Println("HTTP_PROXY not configured, using direct connection")
	}

	// 检查 GitHub Token 配置
	if githubToken != "" {
		log.Println("GITHUB_TOKEN configured: GitHub downloads will use server token for higher rate limits")
	} else {
		log.Println("GITHUB_TOKEN not configured: GitHub downloads will be unauthenticated (60 req/h limit)")
	}

	// 设置路由
	mux := http.NewServeMux()
	mux.HandleFunc("/", proxyHandler)

	// 使用 http.Server 以便支持 graceful shutdown
	srv := &http.Server{
		Addr:    "0.0.0.0:" + getPort(),
		Handler: mux,
	}

	// 优雅关闭：捕获 SIGTERM/SIGINT。ListenAndServe 在 Shutdown 开始时就会返回，
	// 所以必须阻塞等待 Shutdown 结束，否则 main 退出会中断进行中的下载
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	go func() {
		log.Printf("Proxy server is running on http://0.0.0.0:%s", getPort())
		log.Printf("Usage: GET http://0.0.0.0:%s/{URL}", getPort())
		log.Printf("Example: GET http://0.0.0.0:%s/http://nginx.org/download/nginx-1.20.2.tar.gz", getPort())
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Failed to start server: %v", err)
		}
	}()

	<-ctx.Done()
	stop()
	log.Println("Shutting down gracefully...")

	// 给正在进行的请求 30 秒完成
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("Graceful shutdown failed: %v", err)
	}
	log.Println("Server stopped.")
}

func getPort() string {
	if envPort := os.Getenv("PORT"); envPort != "" {
		return envPort
	}
	return "3000"
}
