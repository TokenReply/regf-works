# Grok/Fireworks/OpenRouter 自动注册平台 - Docker 部署

## 快速启动

### 构建镜像
```bash
docker build -t grok-fireworks-openrouter:latest .
```

### 运行容器
```bash
docker run -d \
  --name reg-server \
  -p 8080:8080 \
  -e AUTH_USERNAME=admin \
  -e AUTH_PASSWORD=your_password \
  grok-fireworks-openrouter:latest
```

访问 `http://localhost:8080`

## 环境变量配置

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `AUTH_USERNAME` | 管理员用户名 | admin |
| `AUTH_PASSWORD` | 管理员密码 | admin123 |
| `AHEM_BASE_URL` | AHEM 邮箱服务地址 | - |
| `YYDS_BASE_URL` | YYDS 邮箱服务地址 | - |
| `YYDS_API_KEY` | YYDS API Key | - |
| `PROXY_DEFAULT` | 默认代理地址 | - |

## 挂载配置文件（可选）

```bash
docker run -d \
  --name reg-server \
  -p 8080:8080 \
  -v $(pwd)/configs/config.yaml:/app/configs/config.yaml \
  grok-fireworks-openrouter:latest
```

## 容器内服务架构

```
容器内部：
├── Turnstile Solver   (127.0.0.1:5072) ← Grok + OpenRouter 共用
├── Fireworks Service  (127.0.0.1:5000)
├── OpenRouter Service (127.0.0.1:5001)
└── Go 主服务          (0.0.0.0:8080) ← 对外暴露
```

## 镜像大小优化

- 使用 multi-stage build（Go 编译阶段 + Python 运行阶段）
- 清理 apt 缓存
- Python 依赖使用 `--no-cache-dir`
- 镜像大小约 **2.5GB**（主要是 Camoufox 浏览器引擎）

## 资源建议

- **CPU**: 2 核心（Turnstile solver 需要浏览器渲染）
- **内存**: 2GB（浏览器 + Python + Go）
- **磁盘**: 5GB

## 故障排查

### 查看日志
```bash
docker logs -f reg-server
```

### 进入容器
```bash
docker exec -it reg-server /bin/bash
```

### 检查服务状态
```bash
# 检查 Turnstile Solver
curl http://localhost:5072/health

# 检查 Fireworks 服务
curl http://localhost:5000/health

# 检查 OpenRouter 服务
curl http://localhost:5001/health

# 检查主服务
curl http://localhost:8080/api/health
```

## 生产部署建议

1. **修改默认密码**（通过环境变量或配置文件）
2. **配置代理**（如果需要访问国际服务）
3. **配置邮箱服务**（AHEM/YYDS/GPTMail/MoeMail）
4. **限制资源**：
   ```bash
   docker run -d \
     --name reg-server \
     --memory=2g \
     --cpus=2 \
     -p 8080:8080 \
     grok-fireworks-openrouter:latest
   ```
5. **使用 Docker Compose**（见 `docker-compose.yml`）

## Docker Compose（推荐）

```yaml
version: '3.8'

services:
  reg-server:
    build: .
    image: grok-fireworks-openrouter:latest
    container_name: reg-server
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      - AUTH_USERNAME=admin
      - AUTH_PASSWORD=your_password_here
      - AHEM_BASE_URL=https://mail.jiuuij.de5.net
      - PROXY_DEFAULT=http://your-proxy:7890
    volumes:
      - ./data:/app/data
    deploy:
      resources:
        limits:
          memory: 2G
          cpus: '2'
```

启动：
```bash
docker-compose up -d
```
