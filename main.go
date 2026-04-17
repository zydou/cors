package main

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
)

// Response 标准响应格式
type Response struct {
	Code   int    `json:"code"`
	Usage  string `json:"usage"`
	Note   string `json:"note"`
	Msg    string `json:"msg,omitempty"`
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

// createHTTPClient 创建支持代理的 HTTP 客户端
func createHTTPClient() *http.Client {
	// 从环境变量获取代理配置
	httpProxy := os.Getenv("HTTP_PROXY")
	var transport *http.Transport

	if httpProxy != "" {
		log.Printf("Using proxy: %s", httpProxy)
		proxyURL, err := url.Parse(httpProxy)
		if err != nil {
			log.Printf("Failed to parse proxy URL: %v, using direct connection", err)
			transport = &http.Transport{}
		} else {
			transport = &http.Transport{
				Proxy: http.ProxyURL(proxyURL),
			}
		}
	} else {
		log.Println("Using direct connection (no proxy configured)")
		transport = &http.Transport{}
	}

	client := &http.Client{
		Timeout:   0, // 无超时限制，适用于大文件下载
		Transport: transport,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return nil // 自动跟随重定向
		},
	}

	return client
}

// proxyHandler 代理请求处理函数
func proxyHandler(w http.ResponseWriter, r *http.Request) {
	// 只允许 GET 请求
	if r.Method != "GET" {
		w.WriteHeader(http.StatusMethodNotAllowed)
		res := Response{
			Code:   405,
			Usage:  "GET Host/{URL}",
			Note:   "Only GET requests are allowed",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 从 URL 中提取代理目标地址
	path := strings.TrimPrefix(r.URL.Path, "/")
	if len(path) == 0 {
		w.WriteHeader(http.StatusOK)
		res := Response{
			Code:   0,
			Usage:  "GET Host/{URL}",
			Note:   "Proxy server for downloading files from external resources",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 解码 URL
	targetUrl, err := url.QueryUnescape(path)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		res := Response{
			Code:   400,
			Usage:  "GET Host/{URL}",
			Note:   "Proxy server for downloading files from external resources",
			Msg:    "URL decoding failed",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 忽略特定请求
	if len(targetUrl) < 3 || !strings.Contains(targetUrl, ".") || targetUrl == "favicon.ico" || targetUrl == "robots.txt" {
		w.WriteHeader(http.StatusBadRequest)
		res := Response{
			Code:   400,
			Usage:  "GET Host/{URL}",
			Note:   "Proxy server for downloading files from external resources",
			Msg:    "Invalid URL",
		}
		json.NewEncoder(w).Encode(res)
		return
	}

	// 修复 URL 协议
	targetUrl = fixUrl(targetUrl)

	log.Printf("Proxying GET request to: %s", targetUrl)

	// 创建支持代理的 HTTP 客户端
	client := createHTTPClient()

	// 发送代理请求
	var proxyResp *http.Response
	proxyResp, err = client.Get(targetUrl)
	if err != nil {
		log.Printf("Error proxying request: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		res := Response{
			Code:   -1,
			Usage:  "GET Host/{URL}",
			Note:   "Proxy server for downloading files from external resources",
			Msg:    err.Error(),
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
	_, err = io.Copy(w, proxyResp.Body)
	if err != nil {
		log.Printf("Error copying response body: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		res := Response{
			Code:   -1,
			Usage:  "GET Host/{URL}",
			Note:   "Proxy server for downloading files from external resources",
			Msg:    "Reading response body failed",
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
