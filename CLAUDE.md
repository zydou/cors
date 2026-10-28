# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## 项目架构

**单文件架构** (`src/main.rs` ~400 行) — 所有逻辑集中在一个文件，无模块拆分。

### 核心组件

| 组件                 | 行号范围 | 说明 |
| -------------------- | -------- | ---- |
| `Config` 结构体      | 40-47    | 运行时配置：端口、HTTP 代理、GitHub Token |
| 常量定义             | 49-86    | 连接池参数、超时、Keep-Alive、hop-by-hop 头列表 |
| `is_github_url()`    | 55-61    | 域名匹配：github.com、*.github.com、*.githubusercontent.com |
| `fix_url()`          | 63-72    | 协议补全，兼容 `https:/host/...` 合并斜杠形式 |
| `create_client()`    | 88-106   | 创建 reqwest Client：连接池、代理、TCP 优化、仅连接超时 |
| `forward_headers()`  | 110-141  | 请求头转发 + 过滤 + GitHub Token 注入 |
| `copy_headers()`     | 143-156  | 响应头透传 + hop-by-hop 过滤 |
| `set_cors_headers()` | 158-176  | 设置 CORS 响应头 |
| `ProxyBody`          | 178-221  | `http_body::Body` 实现，桥接 reqwest bytes_stream 到 Axum |
| `root_handler()`     | 223-232  | 根路径返回 API 信息 |
| `proxy_handler()`    | 234-358  | 主代理逻辑：解析 URL、创建客户端、转发、流式返回 |
| `main()`             | 360-416  | 初始化、路由、CORS 层、优雅关闭 |
| 单元测试             | 418-552  | 9 个测试覆盖核心工具函数 |

## 关键设计决策

### 1. 每次请求创建新 Client

`proxy_handler` 内部调用 `create_client(&config)`，每个请求独立创建 Client。

- **优点**：无状态共享问题、配置热加载简单
- **缺点**：连接池无法跨请求复用、TCP 握手开销大
- **优化方向**：改为全局单例 `Arc<ReqwestClient>` 存入 State

### 2. 无整体超时

仅设置 `connect_timeout(30s)`，移除 `timeout()` 总超时。

- 支持任意大小文件下载
- 依赖 TCP Keep-Alive (30s) 检测死连接
- 客户端断开通过 `Context` 传播取消上游请求

### 3. 双向 hop-by-hop 头过滤

请求/响应双向过滤 RFC 7230 定义的 hop-by-hop 头：

```text
connection, transfer-encoding, te, trailer, upgrade,
proxy-authorization, proxy-connection, keep-alive
```

请求端额外删除 `Referer`（绕过防盗链）和 `Host`。

### 4. GitHub Token 注入策略

```rust
if dst.get(AUTHORIZATION).is_none()  // 客户端未带 Authorization
    && is_github_url(target_host)    // 目标是 GitHub 域名
    && github_token.is_some()        // 服务端配置了 Token
```

客户端显式提供的 `Authorization` 优先级最高。

### 5. CORS 双重保障

- `tower-http::CorsLayer` 处理预检请求（OPTIONS 自动返回 200）
- `set_cors_headers()` 在实际响应中手动添加 CORS 头
- 确保流式响应也带有 CORS 头

### 6. 流式代理实现

`ProxyBody` 实现 `http_body::Body`，将 `reqwest::Response::bytes_stream()` (Stream<Item=Result<Bytes>>) 适配为 Axum 可用的 `Body`。避免全量缓冲，支持超大文件。

## 常用开发命令

```bash
# 编译发布版
cargo build --release

# 运行测试
cargo test

# 代码风格检查
cargo fmt --check
cargo clippy

# 运行（开发模式）
cargo run

# 运行（指定环境变量）
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 cargo run
```

## 依赖概览 (Cargo.toml)

| 依赖                   | 版本        | 用途 |
| ---------------------- | ----------- | ---- |
| `axum`                 | 0.8         | Web 框架、路由、状态管理 |
| `tower-http`           | 0.7 (cors)  | CORS 中间件 |
| `reqwest`              | 0.13        | HTTP 客户端（连接池、代理、TLS、流式） |
| `tokio`                | 1.54 (full) | 异步运行时、信号、TCP 监听 |
| `bytes`                | 1.12        | 字节流处理、零拷贝 |
| `serde` / `serde_json` | 1.0         | JSON 序列化 |
| `http-body`            | 1.1         | Body trait、Frame 抽象 |
| `futures-core`         | 0.3         | Stream trait |

## Release Profile 优化

```toml
[profile.release]
lto = true           # 链接时优化
codegen-units = 1    # 单代码生成单元，最大化优化
strip = true         # 剥离符号表
panic = "abort"      # panic 直接终止，减小二进制
```

## 测试策略

- **单元测试内嵌**：`#[cfg(test)] mod tests` 在 `main.rs` 底部
- **覆盖范围**：URL 修复、GitHub 域名判断、头部转发（含大小写、Token 注入优先级、非 GitHub 不注入）、头部复制、CORS 头设置
- **运行**：`cargo test`（9 个测试全部通过）

## 环境变量

| 变量           | 类型   | 默认值 | 说明 |
| -------------- | ------ | ------ | ---- |
| `PORT`         | u16    | 3000   | 监听端口 |
| `HTTP_PROXY`   | String | None   | 上游代理 URL |
| `GITHUB_TOKEN` | String | None   | GitHub PAT |

## API 契约

### 入口点

- `GET /` → `root_handler` 返回 JSON
- `GET /{*path}` → `proxy_handler` 代理转发
- `OPTIONS /*` → `CorsLayer` 自动处理

### URL 解析规则 (`fix_url`)

| 输入                  | 输出 |
| --------------------- | ---- |
| `https://example.com` | `https://example.com` |
| `https:/example.com`  | `https://example.com` |
| `example.com/path`    | `http://example.com/path` |

查询字符串从原请求 URI 直接提取并拼接。

### 错误响应格式

```json
{
  "code": -1,
  "usage": "GET Host/{URL}",
  "note": "Proxy server for downloading files from external resources",
  "msg": "error details"
}
```

## 扩展建议

1. **全局 Client 单例** — 将 `ReqwestClient` 移入 `Config` 或单独 State，避免每请求创建
2. **配置热重载** — 使用 `Arc<RwLock<Config>>` 支持运行时修改代理/Token
3. **指标监控** — 添加请求计数、延迟、错误率（Prometheus metrics）
4. **访问日志** — 结构化日志记录（target、status、duration、bytes）
5. **限流/熔断** — 令牌桶限流、上游熔断保护
6. **HTTPS 证书验证配置** — 可选跳过证书验证（内网自签证书场景）

## 注意事项

- **Rust 版本**：需要 1.75+（Edition 2024）
- **无外部配置文件**：全部通过环境变量
- **单二进制部署**：`cargo build --release` 产出单文件，无运行时依赖
- **优雅关闭**：`axum::serve.with_graceful_shutdown` 等待 30s（默认），处理进行中请求
