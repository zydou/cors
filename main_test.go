package main

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
	target, _ := url.Parse("https://example.com/test")
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
			forwardHeaders(dst, tt.input, target)

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

func TestForwardHeadersGitHubToken(t *testing.T) {
	old := githubToken
	githubToken = "server-token"
	t.Cleanup(func() { githubToken = old })

	tests := []struct {
		name   string
		target string
		input  http.Header
		want   string
	}{
		{"github target gets server token", "https://api.github.com/repos", http.Header{}, "Bearer server-token"},
		{"client auth takes precedence", "https://github.com/x", http.Header{"Authorization": {"Bearer client"}}, "Bearer client"},
		{"non-github target untouched", "https://example.com/x", http.Header{}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			u, _ := url.Parse(tt.target)
			dst := make(http.Header)
			forwardHeaders(dst, tt.input, u)
			if got := dst.Get("Authorization"); got != tt.want {
				t.Errorf("Authorization = %q, want %q", got, tt.want)
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
	rec := httptest.NewRecorder()
	fw := flushWriter{w: rec, rc: http.NewResponseController(rec)}
	if n, err := fw.Write([]byte("hello")); n != 5 || err != nil {
		t.Fatalf("Write = (%d, %v), want (5, nil)", n, err)
	}
	if !rec.Flushed {
		t.Error("expected Flush after successful write")
	}

	// 写入出错时不应 Flush
	rec = httptest.NewRecorder()
	fw = flushWriter{w: errWriter{}, rc: http.NewResponseController(rec)}
	if _, err := fw.Write([]byte("x")); err == nil {
		t.Fatal("Write on failing writer should error")
	}
	if rec.Flushed {
		t.Error("Flush must not happen after failed write")
	}
}

type errWriter struct{}

func (errWriter) Write(p []byte) (int, error) { return 0, errors.New("write failed") }

// ═══════════════════════════════════════════════════════════════
// proxyHandler — 集成测试：起假上游，经代理访问，验证行为
// ═══════════════════════════════════════════════════════════════

func TestProxyHandler_IndexPage(t *testing.T) {
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
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", ct)
	}
}

func TestProxyHandler_InvalidURL(t *testing.T) {
	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	for _, path := range []string{"/ftp://example.com/file", "/http://"} {
		resp, err := http.Get(proxy.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("path %s: status = %d, want 400", path, resp.StatusCode)
		}
	}
}

// TestProxyHandler_Forwarding 验证完整转发链路：状态码、body、Range、CORS，以及 Referer 被删除
func TestProxyHandler_Forwarding(t *testing.T) {
	content := []byte("hello world")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Referer") != "" {
			t.Error("Referer should be stripped before reaching upstream")
		}
		http.ServeContent(w, r, "file.bin", time.Time{}, bytes.NewReader(content))
	}))
	defer upstream.Close()

	proxy := httptest.NewServer(http.HandlerFunc(proxyHandler))
	defer proxy.Close()

	req, _ := http.NewRequest(http.MethodGet, proxy.URL+"/"+upstream.URL+"/file.bin", nil)
	req.Header.Set("Range", "bytes=0-4")
	req.Header.Set("Referer", "http://secret.example")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusPartialContent {
		t.Errorf("status = %d, want 206", resp.StatusCode)
	}
	if string(body) != "hello" {
		t.Errorf("body = %q, want %q", body, "hello")
	}
	if cr := resp.Header.Get("Content-Range"); cr != "bytes 0-4/11" {
		t.Errorf("Content-Range = %q, want %q", cr, "bytes 0-4/11")
	}
	if ao := resp.Header.Get("Access-Control-Allow-Origin"); ao != "*" {
		t.Errorf("Access-Control-Allow-Origin = %q, want *", ao)
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
