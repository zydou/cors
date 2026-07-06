# HTTP Proxy Server

一个高性能 HTTP 代理服务器，专门用于在国内服务器下载海外资源（GitHub、NPM、CDN 等）。

## 功能特点

- ✅ 多连接并行下载 — 大文件自动分块并行下载，充分利用带宽
- ✅ CORS 跨域支持 — 浏览器可直接访问，无需后端中转
- ✅ 客户端头转发 — User-Agent、Accept、Cookie 等全部透传，Referer 自动删除（绕过防盗链）
- ✅ 自动跟随重定向
- ✅ 支持 HTTP 代理配置（通过环境变量）
- ✅ 完整的错误处理
- ✅ 内核级 Socket Buffer 调优（2MB），适配跨境高延迟链路
- ✅ 周期性 Flush，降低客户端感知延迟
- ✅ 轻量级、高性能，零外部依赖
- ✅ 无超时限制，适用于下载超大文件

## 性能优化

| 优化项 | 说明 |
|--------|------|
| 多连接并行 Range 下载 | ≥2MB 文件自动使用 8 连接并行分块下载（可调） |
| 内核 Socket Buffer 2MB | 通过 `setsockopt(SO_RCVBUF/SO_SNDBUF)` 设置，适配高 BDP 链路 |
| 用户态 bufio 512KB | 与 copy buffer 对齐，减少 syscall 次数 |
| Copy Buffer 池化 | `sync.Pool` 复用 512KB buffer，降低 GC 压力 |
| 周期性 Flush | 100ms 间隔推送数据到客户端 |
| Connection Pool 复用 | 全局单例 HTTP client，连接池真正生效 |
| TCP Keep-Alive | 30s 间隔检测死连接 |
| Context 传播 | 客户端断开时自动取消所有下载 goroutine |
| 客户端头转发 | User-Agent/Accept/Cookie 等透传，Referer 自动删除绕过防盗链 |

## 使用场景

- 国内服务器下载海外资源（GitHub releases、NPM packages、Docker images 等）
- 跨境高延迟链路的文件下载加速
- 简单的文件下载代理服务

## 配置说明

### 环境变量

| 变量 | 默认值 | 说明 |
|------|--------|------|
| `PORT` | `3000` | 服务器监听端口 |
| `HTTP_PROXY` | (none) | 上游 HTTP 代理地址 |
| `PARALLEL_CONNS` | `8` | 并行下载连接数（范围 `1`–`16`，设为 `1` 禁用并行） |

## 快速开始

### 编译运行

```bash
# 编译
go build -o cors main.go

# 运行（默认配置）
./cors

# 自定义配置
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 PARALLEL_CONNS=8 ./cors
```

### 使用

```bash
# 基本用法
curl -O http://localhost:3000/https://nginx.org/download/nginx-1.25.4.tar.gz

# 通过代理访问
curl -O http://your-server:3000/https://github.com/user/repo/releases/download/v1.0/file.tar.gz
```

## Docker 部署

```bash
docker run -d -p 3000:3000 \
  -e HTTP_PROXY=http://127.0.0.1:7890 \
  -e PARALLEL_CONNS=8 \
  ghcr.io/zydou/cors:latest
```

## 生产部署

### systemd (Linux)

```bash
sudo cat > /etc/systemd/system/cors.service <<EOF
[Unit]
Description=HTTP Proxy Server
After=network.target

[Service]
Type=simple
User=nobody
Environment="HTTP_PROXY=http://127.0.0.1:7890"
Environment="PORT=3000"
Environment="PARALLEL_CONNS=8"
ExecStart=/usr/local/bin/cors
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

sudo mv cors /usr/local/bin/
sudo chmod +x /usr/local/bin/cors
sudo systemctl daemon-reload
sudo systemctl start cors
sudo systemctl enable cors
```

### Docker Compose

```yaml
version: '3.8'
services:
  cors:
    container_name: cors
    image: ghcr.io/zydou/cors:latest
    ports:
      - "3000:3000"
    environment:
      - PORT=3000
      - HTTP_PROXY=http://127.0.0.1:7890
      - PARALLEL_CONNS=8
    restart: unless-stopped
```

## License

[MIT](LICENSE)
