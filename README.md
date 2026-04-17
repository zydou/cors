# HTTP Proxy Server

一个简单易用的 HTTP 代理服务器，专门用于下载海外资源。

## 功能特点

- ✅ 只支持 GET 请求，专门用于下载文件
- ✅ 自动跟随重定向
- ✅ 支持 HTTP 代理配置（通过环境变量）
- ✅ 完整的错误处理
- ✅ 轻量级、高性能
- ✅ 易于部署和维护
- ✅ 无超时限制，适用于下载非常大的文件

## 使用场景

- 国内服务器下载海外资源（如 GitHub、NPM 等）
- 需要代理的下载请求中转
- 简单的文件下载代理服务

## 配置说明

### 代理配置

通过环境变量配置 HTTP 代理：

```bash
# 使用 HTTP 代理
export HTTP_PROXY=http://127.0.0.1:7890

# 或者使用 HTTPS 代理（如果支持）
export HTTPS_PROXY=http://127.0.0.1:7890
```

### 端口配置

通过环境变量配置服务器端口：

```bash
export PORT=8080
```

## Docker 部署

### 构建和运行 Docker 镜像

```bash
# 构建 Docker 镜像
docker build -t cors-proxy .

# 运行 Docker 容器
docker run -d -p 3000:3000 --name cors-container cors-proxy

# 使用自定义端口和代理运行
docker run -d -p 8080:8080 \
  -e PORT=8080 \
  -e HTTP_PROXY=http://127.0.0.1:7890 \
  --name cors-container cors-proxy
```

### 使用 Docker Compose

创建 `docker-compose.yml` 文件：

```yaml
version: '3.8'
services:
  cors-proxy:
    build: .
    container_name: cors-proxy
    ports:
      - "3000:3000"
    environment:
      - PORT=3000
      - HTTP_PROXY=http://127.0.0.1:7890
    restart: unless-stopped
```

然后运行：

```bash
docker-compose up -d
```

## 编译和运行

### 编译

```bash
# 编译为当前系统架构的可执行文件
go build -o cors main.go

# 编译为 Linux 系统的可执行文件（跨平台编译）
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o cors-linux-amd64 main.go

# 编译为 macOS 系统的可执行文件（跨平台编译）
CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build -o cors-darwin-arm64 main.go

# 编译为 Windows 系统的可执行文件（跨平台编译）
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -o cors-windows-amd64.exe main.go
```

### 运行

```bash
# 直接运行
./cors

# 在后台运行（Linux/macOS）
nohup ./cors > proxy.log 2>&1 &

# 使用代理运行
HTTP_PROXY=http://127.0.0.1:7890 ./cors

# 使用代理和指定端口运行
HTTP_PROXY=http://127.0.0.1:7890 PORT=8080 ./cors

# 停止服务器
pkill -f cors
```

## 使用方法

### 基本用法

```http
GET http://your-server-address/{目标URL}
```

### 示例

要下载 `http://nginx.org/download/nginx-1.20.2.tar.gz`，可以请求：

```http
GET http://0.0.0.0:3000/http://nginx.org/download/nginx-1.20.2.tar.gz
```

或者使用服务器的实际IP地址或域名：

```http
GET http://your-server-ip:3000/http://nginx.org/download/nginx-1.20.2.tar.gz
```

### 实际场景

1. 部署服务器到国内服务器
2. 配置代理（如使用 `HTTP_PROXY=http://127.0.0.1:7890`）
3. 访问
   `http://your-domain.com/http://nginx.org/download/nginx-1.20.2.tar.gz`
   下载文件

## 生产环境部署（Linux）

### 1. 创建系统服务

```bash
# 创建服务文件
sudo cat > /etc/systemd/system/cors.service <<EOF
[Unit]
Description=HTTP Proxy Server
After=network.target

[Service]
Type=simple
User=nobody
Environment="HTTP_PROXY=http://127.0.0.1:7890"
Environment="PORT=3000"
ExecStart=/usr/local/bin/cors
Restart=always
RestartSec=10

[Install]
WantedBy=multi-user.target
EOF

# 移动二进制文件到系统目录
sudo mv cors /usr/local/bin/
sudo chmod +x /usr/local/bin/cors

# 启动服务
sudo systemctl daemon-reload
sudo systemctl start cors
sudo systemctl enable cors

# 检查服务状态
sudo systemctl status cors
```

### 2. 配置防火墙

```bash
# 允许 3000 端口
sudo ufw allow 3000/tcp
```

## 安全注意事项

1. **限制访问**：只允许信任的客户端访问
2. **加密传输**：使用 HTTPS（推荐使用 Nginx 反向代理）
3. **请求验证**：添加请求频率限制
4. **日志记录**：记录访问日志以便分析

## 性能优化建议

1. **增大超时时间**：适用于大文件下载
2. **启用 Gzip 压缩**：减少传输数据量（如果服务支持）
3. **添加访问限制**：防止滥用
4. **监控和日志**：定期检查运行状态

## 故障排除

### 常见问题

1. **无法访问代理服务器**：
   - 检查防火墙设置
   - 确认服务器正在运行
   - 检查端口是否被占用

2. **代理请求失败**：
   - 检查目标 URL 是否正确
   - 确认网络连接
   - 查看服务器日志
   - 检查代理配置是否正确

3. **下载速度慢**：
   - 检查网络连接
   - 确认代理服务器速度
   - 尝试其他代理地址

## 许可证

MIT License
