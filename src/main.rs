use std::env;
use std::net::SocketAddr;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use axum::{
    body::Body,
    extract::{Request, State},
    http::{
        header::{self, HeaderMap, HeaderName, HeaderValue},
        Method, StatusCode, Uri,
    },
    response::{IntoResponse, Response},
    routing::any,
    Router,
};
use bytes::Bytes;
use futures_core::Stream;
use reqwest::Client as ReqwestClient;
use tokio::net::TcpListener;
use tokio::signal;
use tower_http::cors::{Any, CorsLayer};

/// 统一的 JSON 响应格式
#[derive(serde::Serialize)]
struct ApiResponse {
    code: i32,
    usage: &'static str,
    note: &'static str,
    #[serde(skip_serializing_if = "Option::is_none")]
    msg: Option<String>,
}

const USAGE: &str = "GET Host/{URL}";
const NOTE: &str = "Proxy server for downloading files from external resources";

/// 全局配置
#[derive(Clone)]
#[allow(dead_code)]
struct Config {
    /// 服务器监听端口
    port: u16,
    http_proxy: Option<String>,
    github_token: Option<String>,
}

/// 连接池配置常量
const MAX_IDLE_CONNS_PER_HOST: usize = 100;
const IDLE_CONN_TIMEOUT: Duration = Duration::from_secs(90);
const DIAL_TIMEOUT: Duration = Duration::from_secs(30);
const KEEP_ALIVE_INTERVAL: Duration = Duration::from_secs(30);

/// 判断目标是否为 GitHub 相关域名
fn is_github_url(host: &str) -> bool {
    let host = host.to_lowercase();
    host == "github.com"
        || host.ends_with(".github.com")
        || host.ends_with(".githubusercontent.com")
}

/// 补全协议前缀，兼容被合并斜杠的 "https:/host/..." 形式
fn fix_url(url: &str) -> String {
    if url.contains("://") {
        return url.to_string();
    }
    if url.contains(":/") {
        return url.replacen(":/", "://", 1);
    }
    format!("http://{}", url)
}

/// Hop-by-hop headers that should not be forwarded
const HOP_BY_HOP_HEADERS: &[&str] = &[
    "connection",
    "transfer-encoding",
    "te",
    "trailer",
    "upgrade",
    "proxy-authorization",
    "proxy-connection",
    "referer",
    "host",
    "keep-alive",
];

/// 创建支持代理和连接池优化的 reqwest 客户端
fn create_client(config: &Config) -> Result<ReqwestClient, Box<dyn std::error::Error + Send + Sync>> {
    let mut builder = ReqwestClient::builder()
        .pool_max_idle_per_host(MAX_IDLE_CONNS_PER_HOST)
        .pool_idle_timeout(IDLE_CONN_TIMEOUT)
        .tcp_keepalive(Some(KEEP_ALIVE_INTERVAL))
        .tcp_nodelay(true)
        .connect_timeout(DIAL_TIMEOUT)
        .timeout(Duration::from_secs(300))
        .https_only(false);

    if let Some(proxy_url) = &config.http_proxy {
        let proxy = reqwest::Proxy::all(proxy_url)?;
        builder = builder.proxy(proxy);
    }

    let client = builder.build()?;
    Ok(client)
}

/// 转发客户端请求头给上游
/// 删除 Referer（绕过防盗链）和 hop-by-hop 头
/// GitHub 目标且客户端未带 Authorization 时，注入服务端 GITHUB_TOKEN
fn forward_headers(
    dst: &mut reqwest::header::HeaderMap,
    src: &HeaderMap,
    target_host: &str,
    github_token: Option<&str>,
) {
    for key in src.keys() {
        let key_lower = key.as_str().to_lowercase();
        if HOP_BY_HOP_HEADERS.iter().any(|&h| h == key_lower) {
            continue;
        }
        for value in src.get_all(key) {
            if let (Ok(header_name), Ok(header_value)) = (
                reqwest::header::HeaderName::from_bytes(key.as_str().as_bytes()),
                reqwest::header::HeaderValue::from_bytes(value.as_bytes()),
            ) {
                dst.append(header_name, header_value);
            }
        }
    }

    if dst.get(reqwest::header::AUTHORIZATION).is_none() {
        if let Some(token) = github_token {
            if is_github_url(target_host) {
                if let Ok(auth_value) = reqwest::header::HeaderValue::from_str(&format!("Bearer {}", token)) {
                    dst.insert(reqwest::header::AUTHORIZATION, auth_value);
                }
            }
        }
    }
}

/// 复制上游响应头到客户端（原样透传，含 Content-Length / Content-Range / Accept-Ranges）
fn copy_headers(dst: &mut HeaderMap, src: &reqwest::header::HeaderMap) {
    for (key, value) in src.iter() {
        let key_lower = key.as_str().to_lowercase();
        if HOP_BY_HOP_HEADERS.iter().any(|&h| h == key_lower) {
            continue;
        }
        if let Ok(header_name) = HeaderName::from_bytes(key.as_str().as_bytes()) {
            if let Ok(header_value) = HeaderValue::from_bytes(value.as_bytes()) {
                dst.append(header_name, header_value);
            }
        }
    }
}

/// 设置 CORS 响应头
fn set_cors_headers(headers: &mut HeaderMap) {
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_ORIGIN,
        HeaderValue::from_static("*"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_METHODS,
        HeaderValue::from_static("GET, OPTIONS"),
    );
    headers.insert(
        header::ACCESS_CONTROL_ALLOW_HEADERS,
        HeaderValue::from_static("*"),
    );
    headers.insert(
        header::ACCESS_CONTROL_MAX_AGE,
        HeaderValue::from_static("86400"),
    );
}

/// 流式代理响应体
struct ProxyBody<S> {
    stream: S,
}

impl<S> ProxyBody<S> {
    fn new(stream: S) -> Self {
        Self { stream }
    }
}

impl<S> http_body::Body for ProxyBody<S>
where
    S: Stream<Item = Result<Bytes, reqwest::Error>> + Unpin + Send + 'static,
{
    type Data = Bytes;
    type Error = Box<dyn std::error::Error + Send + Sync>;

    fn poll_frame(
        mut self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<http_body::Frame<Self::Data>, Self::Error>>> {
        match Pin::new(&mut self.stream).poll_next(cx) {
            Poll::Ready(Some(Ok(bytes))) => {
                if bytes.is_empty() {
                    Poll::Ready(None)
                } else {
                    Poll::Ready(Some(Ok(http_body::Frame::data(bytes))))
                }
            }
            Poll::Ready(Some(Err(e))) => Poll::Ready(Some(Err(Box::new(e)))),
            Poll::Ready(None) => Poll::Ready(None),
            Poll::Pending => Poll::Pending,
        }
    }

    fn is_end_stream(&self) -> bool {
        false
    }

    fn size_hint(&self) -> http_body::SizeHint {
        http_body::SizeHint::default()
    }
}

/// 根路径处理器 - 返回 API 信息
async fn root_handler() -> impl IntoResponse {
    let body = serde_json::to_string(&ApiResponse {
        code: 0,
        usage: USAGE,
        note: NOTE,
        msg: None,
    }).unwrap();
    (StatusCode::OK, Body::from(body))
}

/// 主代理处理器
async fn proxy_handler(
    State(config): State<Arc<Config>>,
    request: Request,
) -> Response {
    let mut response_headers = HeaderMap::new();
    set_cors_headers(&mut response_headers);

    if request.method() == Method::OPTIONS {
        return (StatusCode::NO_CONTENT, response_headers).into_response();
    }

    if request.method() != Method::GET {
        let body = serde_json::to_string(&ApiResponse {
            code: StatusCode::METHOD_NOT_ALLOWED.as_u16() as i32,
            usage: USAGE,
            note: NOTE,
            msg: Some("Only GET requests are allowed".to_string()),
        }).unwrap();
        return (StatusCode::METHOD_NOT_ALLOWED, response_headers, Body::from(body)).into_response();
    }

    let path = request.uri().path();
    let target = path.strip_prefix('/').unwrap_or("");

    if target.is_empty() {
        let body = serde_json::to_string(&ApiResponse {
            code: 0,
            usage: USAGE,
            note: NOTE,
            msg: None,
        }).unwrap();
        return (StatusCode::OK, response_headers, Body::from(body)).into_response();
    }

    let mut target_url = fix_url(target);

    if let Some(query) = request.uri().query() {
        target_url.push('?');
        target_url.push_str(query);
    }

    let target_uri: Uri = match target_url.parse() {
        Ok(uri) => uri,
        Err(_) => {
            let body = serde_json::to_string(&ApiResponse {
                code: StatusCode::BAD_REQUEST.as_u16() as i32,
                usage: USAGE,
                note: NOTE,
                msg: Some("Invalid URL".to_string()),
            }).unwrap();
            return (StatusCode::BAD_REQUEST, response_headers, Body::from(body)).into_response();
        }
    };

    if target_uri.scheme_str().map(|s| s != "http" && s != "https").unwrap_or(true)
        || target_uri.host().is_none() {
        let body = serde_json::to_string(&ApiResponse {
            code: StatusCode::BAD_REQUEST.as_u16() as i32,
            usage: USAGE,
            note: NOTE,
            msg: Some("Invalid URL".to_string()),
        }).unwrap();
        return (StatusCode::BAD_REQUEST, response_headers, Body::from(body)).into_response();
    }

    let target_host = target_uri.host().unwrap().to_string();
    println!("Proxying GET request to: {}", target_uri);

    let client = match create_client(&config) {
        Ok(c) => c,
        Err(e) => {
            eprintln!("Failed to create HTTP client: {}", e);
            let body = serde_json::to_string(&ApiResponse {
                code: -1i32,
                usage: USAGE,
                note: NOTE,
                msg: Some(e.to_string()),
            }).unwrap();
            return (StatusCode::INTERNAL_SERVER_ERROR, response_headers, Body::from(body)).into_response();
        }
    };

    let mut upstream_req = client.get(target_uri.to_string());

    let mut req_headers = reqwest::header::HeaderMap::new();
    forward_headers(
        &mut req_headers,
        request.headers(),
        &target_host,
        config.github_token.as_deref(),
    );
    upstream_req = upstream_req.headers(req_headers);

    let upstream_resp = match upstream_req.send().await {
        Ok(resp) => resp,
        Err(e) => {
            eprintln!("Error proxying request: {}", e);
            let body = serde_json::to_string(&ApiResponse {
                code: -1i32,
                usage: USAGE,
                note: NOTE,
                msg: Some(e.to_string()),
            }).unwrap();
            return (StatusCode::INTERNAL_SERVER_ERROR, response_headers, Body::from(body)).into_response();
        }
    };

    let upstream_status = upstream_resp.status();
    let upstream_headers = upstream_resp.headers().clone();

    copy_headers(&mut response_headers, &upstream_headers);
    set_cors_headers(&mut response_headers);

    let upstream_body = upstream_resp.bytes_stream();
    let proxy_body = ProxyBody::new(upstream_body);

    let mut response = Response::builder()
        .status(upstream_status)
        .body(Body::new(proxy_body))
        .unwrap();

    *response.headers_mut() = response_headers;
    response
}

#[tokio::main]
async fn main() -> Result<(), Box<dyn std::error::Error + Send + Sync>> {
    let port = env::var("PORT")
        .ok()
        .and_then(|p| p.parse().ok())
        .unwrap_or(3000);

    let http_proxy = env::var("HTTP_PROXY").ok();
    let github_token = env::var("GITHUB_TOKEN").ok();

    let config = Arc::new(Config {
        port,
        http_proxy: http_proxy.clone(),
        github_token: github_token.clone(),
    });

    if let Some(ref proxy) = http_proxy {
        println!("HTTP_PROXY configured: {}", proxy);
    } else {
        println!("HTTP_PROXY not configured, using direct connection");
    }

    if github_token.is_some() {
        println!("GITHUB_TOKEN configured: GitHub downloads will use server token for higher rate limits");
    } else {
        println!("GITHUB_TOKEN not configured: GitHub downloads will be unauthenticated (60 req/h limit)");
    }

    let cors_layer = CorsLayer::new()
        .allow_origin(Any)
        .allow_methods([Method::GET, Method::OPTIONS])
        .allow_headers(Any)
        .max_age(Duration::from_secs(86400));

    let app = Router::new()
        .route("/", any(root_handler))
        .route("/{*path}", any(proxy_handler))
        .layer(cors_layer)
        .with_state(config);

    let addr = SocketAddr::from(([0, 0, 0, 0], port));
    let listener = TcpListener::bind(addr).await?;

    println!("Proxy server is running on http://{}", addr);

    let shutdown = async {
        signal::ctrl_c().await.ok();
        println!("Shutting down gracefully...");
    };

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown)
        .await?;

    println!("Server stopped.");
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::http::HeaderMap;

    #[test]
    fn test_fix_url() {
        let tests = vec![
            ("http://example.com", "http://example.com"),
            ("https://example.com/file.zip", "https://example.com/file.zip"),
            ("http:/example.com", "http://example.com"),
            ("example.com/file.zip", "http://example.com/file.zip"),
            ("example.com", "http://example.com"),
        ];

        for (input, expected) in tests {
            assert_eq!(fix_url(input), expected, "fix_url({}) failed", input);
        }
    }

    #[test]
    fn test_is_github_url() {
        assert!(is_github_url("github.com"));
        assert!(is_github_url("api.github.com"));
        assert!(is_github_url("raw.githubusercontent.com"));
        assert!(is_github_url("GITHUB.COM"));
        assert!(is_github_url("user.github.com"));
        assert!(is_github_url("user.githubusercontent.com"));
        assert!(!is_github_url("example.com"));
        assert!(!is_github_url("gitlab.com"));
        assert!(!is_github_url("github.com.evil.com"));
    }

    #[test]
    fn test_forward_headers() {
        let mut src = HeaderMap::new();
        src.insert("User-Agent", HeaderValue::from_static("test-agent"));
        src.insert("Accept", HeaderValue::from_static("application/json"));
        src.insert("Referer", HeaderValue::from_static("http://secret.com"));
        src.insert("Host", HeaderValue::from_static("evil.com"));
        src.insert("Connection", HeaderValue::from_static("keep-alive"));
        src.insert("Custom-Header", HeaderValue::from_static("custom-value"));

        let mut dst = reqwest::header::HeaderMap::new();
        forward_headers(&mut dst, &src, "example.com", None);

        assert_eq!(dst.get("user-agent").unwrap(), "test-agent");
        assert_eq!(dst.get("accept").unwrap(), "application/json");
        assert_eq!(dst.get("custom-header").unwrap(), "custom-value");
        assert!(dst.get("referer").is_none());
        assert!(dst.get("host").is_none());
        assert!(dst.get("connection").is_none());
    }

    #[test]
    fn test_forward_headers_case_insensitive() {
        let mut src = HeaderMap::new();
        src.insert("referer", HeaderValue::from_static("http://a.com"));
        src.insert("HOST", HeaderValue::from_static("evil.com"));
        src.insert("User-Agent", HeaderValue::from_static("ok"));

        let mut dst = reqwest::header::HeaderMap::new();
        forward_headers(&mut dst, &src, "example.com", None);

        assert_eq!(dst.get("user-agent").unwrap(), "ok");
        assert!(dst.get("referer").is_none());
        assert!(dst.get("host").is_none());
    }

    #[test]
    fn test_forward_headers_github_token() {
        let mut src = HeaderMap::new();
        src.insert("User-Agent", HeaderValue::from_static("test"));

        let mut dst = reqwest::header::HeaderMap::new();
        forward_headers(&mut dst, &src, "github.com", Some("server-token"));

        assert_eq!(dst.get("authorization").unwrap(), "Bearer server-token");
        assert_eq!(dst.get("user-agent").unwrap(), "test");
    }

    #[test]
    fn test_forward_headers_github_token_client_priority() {
        let mut src = HeaderMap::new();
        src.insert("Authorization", HeaderValue::from_static("Bearer client-token"));
        src.insert("User-Agent", HeaderValue::from_static("test"));

        let mut dst = reqwest::header::HeaderMap::new();
        forward_headers(&mut dst, &src, "github.com", Some("server-token"));

        assert_eq!(dst.get("authorization").unwrap(), "Bearer client-token");
    }

    #[test]
    fn test_forward_headers_non_github_no_token() {
        let mut src = HeaderMap::new();
        src.insert("User-Agent", HeaderValue::from_static("test"));

        let mut dst = reqwest::header::HeaderMap::new();
        forward_headers(&mut dst, &src, "example.com", Some("server-token"));

        assert!(dst.get("authorization").is_none());
    }

    #[test]
    fn test_copy_headers() {
        let mut src = reqwest::header::HeaderMap::new();
        src.insert("content-type", "application/octet-stream".parse().unwrap());
        src.insert("content-length", "12345".parse().unwrap());
        src.insert("accept-ranges", "bytes".parse().unwrap());
        src.insert("content-range", "bytes 0-99/12345".parse().unwrap());
        src.insert("x-custom", "custom-value".parse().unwrap());
        src.insert("connection", "keep-alive".parse().unwrap());

        let mut dst = HeaderMap::new();
        copy_headers(&mut dst, &src);

        assert_eq!(dst.get("content-type").unwrap(), "application/octet-stream");
        assert_eq!(dst.get("content-length").unwrap(), "12345");
        assert_eq!(dst.get("accept-ranges").unwrap(), "bytes");
        assert_eq!(dst.get("content-range").unwrap(), "bytes 0-99/12345");
        assert_eq!(dst.get("x-custom").unwrap(), "custom-value");
        assert!(dst.get("connection").is_none());
    }

    #[test]
    fn test_set_cors_headers() {
        let mut headers = HeaderMap::new();
        set_cors_headers(&mut headers);

        assert_eq!(headers.get("access-control-allow-origin").unwrap(), "*");
        assert_eq!(headers.get("access-control-allow-methods").unwrap(), "GET, OPTIONS");
        assert_eq!(headers.get("access-control-allow-headers").unwrap(), "*");
        assert_eq!(headers.get("access-control-max-age").unwrap(), "86400");
    }
}