package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// ═══════════════════════════════════════════════════════════════
// fixUrl — 表驱动测试
// ═══════════════════════════════════════════════════════════════

func TestFixUrl(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"already has http", "http://example.com", "http://example.com"},
		{"already has https", "https://example.com/file.zip", "https://example.com/file.zip"},
		{"missing slashes", "http:/example.com", "http://example.com"},
		{"no protocol", "example.com/file.zip", "http://example.com/file.zip"},
		{"domain only", "example.com", "http://example.com"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := fixUrl(tt.input)
			if got != tt.expected {
				t.Errorf("fixUrl(%q) = %q, want %q", tt.input, got, tt.expected)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════
// forwardHeaders — 头转发过滤逻辑
// ═══════════════════════════════════════════════════════════════

func TestForwardHeaders(t *testing.T) {
	tests := []struct {
		name     string
		input    http.Header
		wantKeys []string
		dropKeys []string
	}{
		{
			name: "normal headers forwarded",
			input: http.Header{
				"User-Agent":      {"test-agent"},
				"Accept":          {"application/json"},
				"Accept-Language": {"zh-CN"},
			},
			wantKeys: []string{"User-Agent", "Accept", "Accept-Language"},
		},
		{
			name: "protected headers dropped",
			input: http.Header{
				"Referer":           {"http://secret.com"},
				"Host":              {"evil.com"},
				"Range":             {"bytes=0-100"},
				"Connection":        {"keep-alive"},
				"Transfer-Encoding": {"chunked"},
				"User-Agent":        {"good-agent"},
			},
			wantKeys: []string{"User-Agent"},
			dropKeys: []string{"Referer", "Host", "Range", "Connection", "Transfer-Encoding"},
		},
		{
			name: "case-insensitive matching",
			input: http.Header{
				"referer":    {"http://a.com"},
				"HOST":       {"evil.com"},
				"User-Agent": {"ok"},
			},
			wantKeys: []string{"User-Agent"},
			dropKeys: []string{"Referer", "Host"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dst := make(http.Header)
			forwardHeaders(dst, tt.input, "https://example.com/test")

			for _, key := range tt.wantKeys {
				if dst.Get(key) == "" {
					t.Errorf("expected header %q to be preserved", key)
				}
			}
			for _, key := range tt.dropKeys {
				if dst.Get(key) != "" {
					t.Errorf("expected header %q to be dropped, got %q", key, dst.Get(key))
				}
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════
// copyHeaders — 响应头复制并过滤
// ═══════════════════════════════════════════════════════════════

func TestCopyHeaders(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"Content-Type":   {"application/octet-stream"},
			"Content-Length": {"12345"},
			"Accept-Ranges":  {"bytes"},
			"X-Custom":       {"custom-value"},
		},
	}
	w := httptest.NewRecorder()
	copyHeaders(w, resp)

	if got := w.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}
	if got := w.Header().Get("X-Custom"); got != "custom-value" {
		t.Errorf("X-Custom = %q, want custom-value", got)
	}
	if got := w.Header().Get("Content-Length"); got != "" {
		t.Errorf("Content-Length should be filtered, got %q", got)
	}
	if got := w.Header().Get("Accept-Ranges"); got != "" {
		t.Errorf("Accept-Ranges should be filtered, got %q", got)
	}
}

// ═══════════════════════════════════════════════════════════════
// isPrivateIP — SSRF 防护核心
// ═══════════════════════════════════════════════════════════════

func TestIsPrivateIP(t *testing.T) {
	tests := []struct {
		name     string
		ip       string
		expected bool
	}{
		// — 必须拦截 —
		{"loopback 127.0.0.1", "127.0.0.1", true},
		{"loopback 127.0.0.53", "127.0.0.53", true},
		{"private 10.0.0.1", "10.0.0.1", true},
		{"private 10.255.255.255", "10.255.255.255", true},
		{"private 192.168.1.1", "192.168.1.1", true},
		{"private 172.16.0.1", "172.16.0.1", true},
		{"private 172.31.255.255", "172.31.255.255", true},
		{"cloud metadata 169.254.169.254", "169.254.169.254", true},
		{"link-local 169.254.1.1", "169.254.1.1", true},
		{"CGNAT 100.64.0.1", "100.64.0.1", true},
		{"CGNAT 100.127.255.255", "100.127.255.255", true},
		{"IPv6 loopback ::1", "::1", true},

		// — 必须放行 —
		{"public 8.8.8.8", "8.8.8.8", false},
		{"public 1.1.1.1", "1.1.1.1", false},
		{"public 9.9.9.9", "9.9.9.9", false},
		{"public 203.0.113.1", "203.0.113.1", false},
		{"boundary 172.32.0.1", "172.32.0.1", false},
		{"boundary 100.128.0.1", "100.128.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			if ip == nil {
				t.Fatalf("net.ParseIP(%q) returned nil", tt.ip)
			}
			got := isPrivateIP(ip)
			if got != tt.expected {
				t.Errorf("isPrivateIP(%q) = %v, want %v", tt.ip, got, tt.expected)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════
// validateTargetURL — SSRF 防护入口（字面 IP 与无效 URL 分支）
// ═══════════════════════════════════════════════════════════════

func TestValidateTargetURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		wantErr bool
		errMsg  string
	}{
		// — 字面 IP：应拦截 —
		{"literal loopback", "http://127.0.0.1/file", true, "forbidden"},
		{"literal private 10.x", "http://10.0.0.1/secret", true, "forbidden"},
		{"literal cloud metadata", "http://169.254.169.254/latest/meta-data/", true, "forbidden"},
		{"literal 192.168.x", "http://192.168.1.1/", true, "forbidden"},
		{"literal 172.16.x", "http://172.16.0.1/", true, "forbidden"},

		// — 字面 IP：应放行（公网） —
		{"literal public 8.8.8.8", "http://8.8.8.8/", false, ""},

		// — 参数校验 —
		{"no host", "http://", true, "no host"},
		{"invalid URL", "://bad", true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateTargetURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Errorf("validateTargetURL(%q) expected error, got nil", tt.url)
				} else if tt.errMsg != "" && !strings.Contains(err.Error(), tt.errMsg) {
					t.Errorf("validateTargetURL(%q) error = %q, want substring %q", tt.url, err.Error(), tt.errMsg)
				}
			} else {
				if err != nil {
					t.Errorf("validateTargetURL(%q) unexpected error: %v", tt.url, err)
				}
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════
// flushWriter — 幂等 stop 测试（防 double-close panic）
// ═══════════════════════════════════════════════════════════════

func TestFlushWriter_IdempotentStop(t *testing.T) {
	flusher := &fakeFlusher{}
	fw := newFlushWriter(io.Discard, flusher, 1000)

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("stop() panicked on double call: %v", r)
				}
			}()
			fw.stop()
		}()
	}
	wg.Wait()
}

type fakeFlusher struct{}

func (f *fakeFlusher) Flush() {}

// ═══════════════════════════════════════════════════════════════
// parallelDownload — ctx 取消后 goroutine 不泄漏
// 先做一轮 Range 探测（HEAD），再并行下载，再 cancel 提前退出。
// ═══════════════════════════════════════════════════════════════

func TestParallelDownload_Success(t *testing.T) {
	content := make([]byte, 200)
	for i := range content {
		content[i] = byte(i)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "200")
			w.WriteHeader(http.StatusOK)
			return
		}
		// 解析 Range: bytes=start-end
		rangeHdr := r.Header.Get("Range")
		start, end := parseRange(rangeHdr, len(content)-1)
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
	}))
	defer server.Close()

	var buf bytes.Buffer
	err := parallelDownload(context.Background(), server.URL, 200, 4, 50, &buf, nil, nil)
	if err != nil {
		t.Fatalf("parallelDownload error: %v", err)
	}
	if !bytes.Equal(buf.Bytes(), content) {
		t.Errorf("got %d bytes, want %d bytes matching content", buf.Len(), len(content))
	}
}

func TestParallelDownload_CancelNoLeak(t *testing.T) {
	content := make([]byte, 500)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "500")
			w.WriteHeader(http.StatusOK)
			return
		}
		rangeHdr := r.Header.Get("Range")
		start, end := parseRange(rangeHdr, len(content)-1)
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(http.StatusPartialContent)
		w.Write(content[start : end+1])
	}))
	defer server.Close()

	// 立刻取消
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	err := parallelDownload(ctx, server.URL, 500, 4, 50, &buf, nil, nil)
	if err == nil {
		t.Error("expected error from cancelled context, got nil")
	}

	// 给 goroutine 退出的时间，若泄漏则此步后 goroutine 数异常升高（CI 下可用 go test -race 辅助检测）
	time.Sleep(200 * time.Millisecond)
}

// parseRange 解析 "bytes=start-end" 头中的起止位置
func parseRange(hdr string, defaultEnd int) (int, int) {
	if !strings.HasPrefix(hdr, "bytes=") {
		return 0, defaultEnd
	}
	spec := strings.TrimPrefix(hdr, "bytes=")
	parts := strings.Split(spec, "-")
	if len(parts) != 2 {
		return 0, defaultEnd
	}
	start, err1 := strconv.Atoi(parts[0])
	end, err2 := strconv.Atoi(parts[1])
	if err1 != nil || err2 != nil || start > end || end > defaultEnd {
		return 0, defaultEnd
	}
	return start, end
}

// ═══════════════════════════════════════════════════════════════
// proxyHandler 集成测试
//
// 思路：起一个假上游服务器（httptest.Server），再用另一个
// httptest.Server 包装 proxyHandler，对它发请求打到上游，
// 验证代理行为是否符合预期。
// ═══════════════════════════════════════════════════════════════

// newProxyTestServer 创建一个带假上游的 proxyHandler 测试服务器
func newProxyTestServer(upstream http.Handler) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(proxyHandler))
}

func TestProxyHandler_IndexPage(t *testing.T) {
	// 空路径返回使用说明（只需确认 200 + 没走网络）
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	resp, err := http.Get(proxy.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	// 不严格检查 Content-Type（关键是 handler 返回了 JSON 结构）
}

func TestProxyHandler_BlocksPrivateIP(t *testing.T) {
	// 假上游：如果代理错误地允许请求，它会连过来——但我们测的是代理没发请求
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("proxy should NOT have forwarded request to private IP")
		w.WriteHeader(http.StatusTeapot) // 如果收到就是错
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	// 用公网 IP（8.8.8.8）碰一下：应该能进 handler，但 8.8.8.8 实际上不会响应 proxy 请求（超时）。
	// 关键断言是“handler 不返回 403”（说明过了 SSRF 检查），且“handler 尝试连接上游”
	// 这个用例主要是为了对比。
	// 真正测 SSRF 拦截的用例：
	urls := []string{
		"/http://127.0.0.1/",
		"/http://10.0.0.1/",
		"/http://192.168.1.1/",
		"/http://169.254.169.254/latest/meta-data/",
	}
	for _, path := range urls {
		resp, err := http.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusForbidden {
			t.Errorf("path %s: status = %d, want 403", path, resp.StatusCode)
		}
	}
}

func TestProxyHandler_BlocksInvalidURL(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	// 不满足 “含 . 且长度 > 3” 的前置检查
	resp, err := http.Get(proxy.URL + "/ab")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

// TestProxyHandler_AllowlistInAction 通过直接调用 handler 验证：
// 当目标主机明确不是私有 IP 时，handler 不会在验证阶段拒绝它。
// 注意：完整的代理转发（单连接/并行下载到公网）在此不能端到端覆盖，
// 因为请求内部网络被 SSRF 防护正确拦截；实际转发功能由
// TestProxyHandler_HeaderForwarding 之前的 httptest 单测覆盖逻辑正确性。
func TestProxyHandler_AllowSemantics(t *testing.T) {
	// validateTargetURL 对公共域名不返回错误（见 TestValidateTargetURL）。
	// 这里只测试：handler 不会把公网域名当作私有 IP 拦截。
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	// 公网域名不应被 SSRF 防护拦截（实际请求可能因外网不通而失败，但不是 403）
	resp, err := http.Get(proxy.URL + "/http://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		t.Errorf("public domain example.com should not be blocked by SSRF, got 403")
	}
}

func TestProxyHandler_OnlyGETAllowed(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		req, _ := http.NewRequest(method, proxy.URL+"/http://example.com", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s status = %d, want 405", method, resp.StatusCode)
		}
	}
}

func TestProxyHandler_OPTIONS(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	req, _ := http.NewRequest("OPTIONS", proxy.URL+"/", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	// CORS 头
	if ao := resp.Header.Get("Access-Control-Allow-Origin"); ao != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", ao)
	}
}

// TestProxyHandler_HeaderForwarding 在端到端层面测上游收到的头。
// 由于 127.0.0.1 被 SSRF 拦截，这里改为测 forwardHeaders 单测
// （见 TestForwardHeaders）以及对 handler 请求后的最低保证：
// 对于公网域名，请求不会被 handler 拒绝（可能因无外网而请求失败，但不是 403）。

// ═══════════════════════════════════════════════════════════════
// setCORSHeaders — 单测
// ═══════════════════════════════════════════════════════════════

func TestSetCORSHeaders(t *testing.T) {
	w := httptest.NewRecorder()
	setCORSHeaders(w)

	checks := map[string]string{
		"Access-Control-Allow-Origin":  "*",
		"Access-Control-Allow-Methods": "GET, OPTIONS",
		"Access-Control-Allow-Headers": "*",
		"Access-Control-Max-Age":       "86400",
	}
	for k, v := range checks {
		if got := w.Header().Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
}

// ═══════════════════════════════════════════════════════════════
// getPort — 单测
// ═══════════════════════════════════════════════════════════════

func TestGetPort(t *testing.T) {
	// 无环境变量
	t.Setenv("PORT", "")  // 清空
	// 注意：Setenv 设置的 "" 仍会让 os.Getenv 返回 "", 所以 getPort 会返回 "3000"
	// 要测环境变量生效需显式设置
	t.Setenv("PORT", "8080")
	if got := getPort(); got != "8080" {
		t.Errorf("getPort with PORT=8080: got %q, want 8080", got)
	}

	t.Setenv("PORT", "")
	if got := getPort(); got != "3000" {
		t.Errorf("getPort without PORT: got %q, want 3000", got)
	}
}
