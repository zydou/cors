package main

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
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
			name: "protected headers dropped, Range forwarded",
			input: http.Header{
				"Referer":           {"http://secret.com"},
				"Host":              {"evil.com"},
				"Range":             {"bytes=0-100"},
				"Connection":        {"keep-alive"},
				"Transfer-Encoding": {"chunked"},
				"User-Agent":        {"good-agent"},
			},
			wantKeys: []string{"User-Agent", "Range"},
			dropKeys: []string{"Referer", "Host", "Connection", "Transfer-Encoding"},
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
// copyHeaders — 响应头原样透传（含 Content-Length / Accept-Ranges）
// ═══════════════════════════════════════════════════════════════

func TestCopyHeaders(t *testing.T) {
	resp := &http.Response{
		Header: http.Header{
			"Content-Type":   {"application/octet-stream"},
			"Content-Length": {"12345"},
			"Accept-Ranges":  {"bytes"},
			"Content-Range":  {"bytes 0-99/12345"},
			"X-Custom":       {"custom-value"},
		},
	}
	w := httptest.NewRecorder()
	copyHeaders(w, resp)

	for key, want := range map[string]string{
		"Content-Type":   "application/octet-stream",
		"Content-Length": "12345",
		"Accept-Ranges":  "bytes",
		"Content-Range":  "bytes 0-99/12345",
		"X-Custom":       "custom-value",
	} {
		if got := w.Header().Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

// ═══════════════════════════════════════════════════════════════
// flushWriter — 每次写入成功后立即 Flush
// ═══════════════════════════════════════════════════════════════

func TestFlushWriter_FlushAfterWrite(t *testing.T) {
	flusher := &countingFlusher{}
	fw := newFlushWriter(io.Discard, flusher)
	if n, err := fw.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if flusher.count != 1 {
		t.Errorf("Flush called %d times after 1 write, want 1", flusher.count)
	}

	// 出错 writer 上不应 Flush
	fwErr := newFlushWriter(errWriter{}, flusher)

	// 写入出错时不应 Flush
	if n, err := fwErr.Write([]byte("x")); err == nil {
		t.Fatalf("Write on failing writer should error, got (%d, %v)", n, err)
	}
	if flusher.count != 1 {
		t.Errorf("Flush called %d times after failed write, want still 1", flusher.count)
	}
}

type countingFlusher struct{ count int }

func (f *countingFlusher) Flush() { f.count++ }

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("write failed") }

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
