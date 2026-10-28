# cors

高性能 HTTP 代理服务器，用于在国内服务器下载海外资源（GitHub、NPM、CDN 等）。

## 功能特点

- ✅ **Range 透传** — 支持断点续传（`curl -C -`）与分片下载
- ✅ **CORS 跨域支持** — 浏览器可直接访问，无需后端中转
- ✅ **客户端头转发** — User-Agent、Accept、Cookie 等全部透传，Referer 自动删除（绕过防盗链）
- ✅ **自动跟随重定向**
- ✅ **支持 HTTP 代理配置**（通过环境变量）
- ✅ **完整的错误处理**
- ✅ **连接池复用** — 全局单例 HTTP client，连接池真正生效
- ✅ **TCP Keep-Alive** — 30s 间隔检测死连接
- ✅ **Context 传播** — 客户端断开时自动取消上游请求
- ✅ **GitHub Token 自动注入** — GitHub 下载自动使用服务端 Token，提高限流额度
- ✅ **无整体超时限制** — 适用于下载超大文件
- ✅ **优雅关闭** — 30s 超时等待进行中下载完成

## 快速开始

### 编译运行

```bash
# 编译（需要 Rust 1.75+）
cargo build --release

# 运行（默认配置：端口 3000）
./target/release/cors

# 自定义配置
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 ./target/release/cors
```

### 使用示例

```bash
# 基本用法
curl -O http://localhost:3000/https://nginx.org/download/nginx-1.25.4.tar.gz

# 通过代理访问
curl -O http://your-server:3000/https://github.com/user/repo/releases/download/v1.0/file.tar.gz

# 断点续传（Range 自动透传给上游）
curl -C - -O http://your-server:3000/https://github.com/user/repo/releases/download/v1.0/file.tar.gz

# 浏览器直接访问（CORS 支持）
# http://your-server:3000/https://cdn.example.com/file.js
```

## 配置说明

### 环境变量

| 变量           | 默认值 | 说明 |
| -------------- | ------ | ---- |
| `PORT`         | `3000` | 服务器监听端口 |
| `HTTP_PROXY`   | (none) | 上游 HTTP 代理地址，如 `http://127.0.0.1:7890` |
| `GITHUB_TOKEN` | (none) | GitHub Personal Access Token，用于提高下载限流额度 |

### GitHub Token 自动注入

当满足以下条件时，自动注入 `Authorization: Bearer <GITHUB_TOKEN>`：

1. 设置了 `GITHUB_TOKEN` 环境变量
2. 客户端请求未携带 `Authorization` 头
3. 目标域名为 `github.com`、`*.github.com` 或 `*.githubusercontent.com`

客户端提供的 `Authorization` 头优先级高于服务端 Token。

## API 端点

### `GET /`

返回 API 信息（JSON 格式）：

```json
{
  "code": 0,
  "usage": "GET Host/{URL}",
  "note": "Proxy server for downloading files from external resources"
}
```

### `GET /{target-url}`

代理请求到目标 URL，目标 URL 作为路径参数传递。

示例：

- `GET /https://example.com/file.zip` → 代理到 `https://example.com/file.zip`
- `GET /http:/example.com/file.zip` → 自动修正为 `http://example.com/file.zip`
- `GET /example.com/file.zip` → 自动补全为 `http://example.com/file.zip`
- `GET /https://api.github.com/repos?foo=bar` → 查询字符串原样透传

### `OPTIONS /` / `OPTIONS /{target-url}`

CORS 预检请求，返回 200 OK 及 CORS 头。

## 响应格式

成功时直接返回上游响应（流式转发，保持原始状态码、头部、Body）。

错误时返回统一 JSON 格式：

```json
{
  "code": 400,
  "usage": "GET Host/{URL}",
  "note": "Proxy server for downloading files from external resources",
  "msg": "Invalid URL"
}
```

| code | 含义 |
| ---- | ---- |
| 0    | 根路径访问成功 |
| 400  | 无效 URL |
| 405  | 方法不允许（仅支持 GET/OPTIONS） |
| 500  | 内部错误 |
| -1   | 上游请求失败 |

## 头部处理

### 请求头转发（客户端 → 上游）

- **转发**: User-Agent, Accept, Accept-Language, Cookie, Range, Authorization 等
- **删除**: Referer, Host, Connection, Transfer-Encoding, TE, Trailer, Upgrade, Proxy-Authorization, Proxy-Connection, Keep-Alive

### 响应头透传（上游 → 客户端）

- **透传**: Content-Type, Content-Length, Content-Range, Accept-Ranges, ETag, Last-Modified 等
- **删除**: hop-by-hop 头

### CORS 响应头（自动添加）

```text
Access-Control-Allow-Origin: *
Access-Control-Allow-Methods: GET, OPTIONS
Access-Control-Allow-Headers: *
Access-Control-Max-Age: 86400
```

## 开发

### 运行测试

```bash
cargo test
```

### 代码风格

```bash
cargo fmt --check
cargo clippy
```

## 性能优化

| 优化项           | 说明 |
| ---------------- | ---- |
| 连接池复用       | 全局单例 HTTP client，`pool_max_idle_per_host=100` |
| TCP Keep-Alive   | 30s 间隔检测死连接 |
| 无整体超时       | 适用于下载超大文件，仅有连接超时 (30s) |
| Context 取消传播 | 客户端断开自动取消上游请求 |
| 头部过滤         | 自动剥离 hop-by-hop 头和 Referer |

## 许可证

[MIT](LICENSE)
