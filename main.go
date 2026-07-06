package main

import (
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Response 标准响应格式
type Response struct {
	Code  int    `json:"code"`
	Usage string `json:"usage"`
	Note  string `json:"note"`
	Msg   string `json:"msg,omitempty"`
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

// 全局复用 HTTP 客户端 —— 连接池真正生效
var client = createHTTPClient()

// copy buffer 池化，减少 GC 压力
var bufPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 128*1024)
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

		// TCP 读写缓冲区调优：减少系统调用次数
		ReadBufferSize:  128 * 1024,
		WriteBufferSize: 128 * 1024,

		// 自定义拨号器：启用 TCP keep-alive，及时检测死连接
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
			DualStack: true,
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
func proxyHandler(w http.ResponseWriter, r *http.Request) {
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
	path := strings.TrimPrefix(r.URL.Path, "/")
	if len(path) == 0 {
		w.WriteHeader(http.StatusOK)
		res := Response{
			Code:  0,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 解码 URL
	targetUrl, err := url.QueryUnescape(path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		res := Response{
			Code:  400,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   "URL decoding failed",
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

	log.Printf("Proxying GET request to: %s", targetUrl)

	// 使用全局复用的 HTTP 客户端
	proxyResp, err := client.Get(targetUrl)
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

	// 复制响应头
	for name, values := range proxyResp.Header {
		for _, value := range values {
			w.Header().Add(name, value)
		}
	}

	// 写入响应状态和内容
	w.WriteHeader(proxyResp.StatusCode)

	// 立即 flush，降低首字节时间（TTFB）
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}

	// 从池中获取 buffer，减少分配
	buf := bufPool.Get().([]byte)
	defer bufPool.Put(buf)

	_, err = io.CopyBuffer(w, proxyResp.Body, buf)
	if err != nil {
		log.Printf("Error copying response body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		res := Response{
			Code:  -1,
			Usage: "GET Host/{URL}",
			Note:  "Proxy server for downloading files from external resources",
			Msg:   "Reading response body failed",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	log.Printf("Successfully proxied %d bytes from: %s", proxyResp.ContentLength, targetUrl)
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

	// 设置路由
	http.HandleFunc("/", proxyHandler)

	// 启动服务器
	PORT := "3000"
	// 允许通过环境变量设置端口
	if envPort := os.Getenv("PORT"); envPort != "" {
		PORT = envPort
	}

	log.Printf("Proxy server is running on http://0.0.0.0:%s", PORT)
	log.Printf("Usage: GET http://0.0.0.0:%s/{URL}", PORT)
	log.Printf("Example: GET http://0.0.0.0:%s/http://nginx.org/download/nginx-1.20.2.tar.gz", PORT)

	err := http.ListenAndServe("0.0.0.0:"+PORT, nil)
	if err != nil {
		log.Fatalf("Failed to start server: %v", err)
	}
}
