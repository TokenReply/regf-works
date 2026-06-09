# regf-works

Grok + Fireworks 多平台账号自动注册工具。

提供两种 Docker 镜像：
- **lite** — 仅 Fireworks 注册，轻量快速（~150MB）
- **full** — Fireworks + Grok 注册，内置 Turnstile 打码服务，开箱即用（~1.5GB）

## Docker 部署

### full 版（推荐，Grok + Fireworks 全功能）

```bash
docker pull ghcr.io/jiujiu532/regf-works:latest

docker run -d \
  --name regf-works \
  -p 8080:8080 \
  -v ./configs:/app/configs \
  ghcr.io/jiujiu532/regf-works:latest
```

内置服务：
- Go HTTP API（:8080）
- Fireworks Python 注册引擎（:5000 内部）
- Camoufox Turnstile solver（:8888 内部）

部署后只需通过 API 配置邮箱服务地址即可使用。

### lite 版（仅 Fireworks）

```bash
docker pull ghcr.io/jiujiu532/regf-works:lite

docker run -d \
  --name regf-works-lite \
  -p 8080:8080 \
  -v ./configs:/app/configs \
  ghcr.io/jiujiu532/regf-works:lite
```

## 配置

首次启动会自动从模板生成配置文件。通过 API 动态修改配置（无需重启）：

### 配置邮箱服务

```bash
curl -X POST http://localhost:8080/api/settings/mail \
  -H "Content-Type: application/json" \
  -d '{
    "provider_priority": "ahem",
    "ahem": {
      "base_url": "https://your-ahem-server.com",
      "domains": ""
    }
  }'
```

`domains` 留空会自动从 AHEM 服务获取所有可用域名，注册时随机选取。

### 配置代理

```bash
curl -X POST http://localhost:8080/api/settings/proxy \
  -H "Content-Type: application/json" \
  -d '{
    "default": "http://user:pass@host:port",
    "pool": ["http://proxy1:port", "http://proxy2:port"]
  }'
```

### 配置文件

也可以直接编辑 `configs/config.yaml`：

```yaml
server:
  port: 8080

proxy:
  default: ""
  pool: []

mail:
  provider_priority: "ahem"
  ahem:
    base_url: "https://your-ahem-server.com"
    domains: ""
  yydsmail:
    base_url: ""
    api_key: ""

turnstile:
  solver_urls:
    - "http://127.0.0.1:8888"    # full 镜像已内置
  capsolver_key: ""               # 可选：付费打码备用
  yescaptcha_key: ""              # 可选：付费打码备用

grok:
  site_key: "0x4AAAAAAAhr9JGVDZbrZOo0"
  action_id: ""                   # 自动扫描获取

fireworks:
  service_url: "http://127.0.0.1:5000"   # full 镜像已内置
  max_concurrent: 10
```

## API 接口

### 注册

```bash
# Grok 注册
curl -N http://localhost:8080/api/grok/register \
  -H "Content-Type: application/json" \
  -d '{"proxy": "http://user:pass@host:port"}'

# Fireworks 注册
curl -N http://localhost:8080/api/fireworks/register \
  -H "Content-Type: application/json" \
  -d '{}'
```

响应为 SSE 流：

```
event: log
data: [*] 任务开始

event: log
data: [+] 邮箱: abc123@example.com (via ahem)

event: result
data: {"ok":true,"email":"abc123@example.com","platform":"grok","data":{"auth_token":"...","password":"..."}}
```

### 完整接口列表

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/health` | 健康检查 |
| POST | `/api/grok/register` | Grok 注册（SSE） |
| POST | `/api/fireworks/register` | Fireworks 注册（SSE） |
| GET | `/api/settings/mail` | 获取邮箱配置 |
| POST | `/api/settings/mail` | 更新邮箱配置 |
| GET | `/api/settings/proxy` | 获取代理配置 |
| POST | `/api/settings/proxy` | 更新代理配置 |

## CLI 模式

```bash
# Grok 单个注册
./reg-cli grok --config configs/config.yaml

# Fireworks 批量注册
./reg-cli fireworks --config configs/config.yaml --count 5 --output results.jsonl

# 指定代理
./reg-cli grok --proxy http://user:pass@host:port
```

## 本地开发

```bash
# Go 服务
make dev

# Fireworks Python 服务（另一个终端）
make fireworks-service

# Turnstile solver（另一个终端，需要 camoufox）
make solver-service
```

## 支持平台

| 平台 | 方式 | 产出 | 镜像 |
|------|------|------|------|
| Grok | gRPC-web + Server Actions + Turnstile | SSO Token, 密码, 支付链接 | full |
| Fireworks | Server Actions + AWS Cognito | API Key, 账号凭据 | lite / full |

## 邮箱 Provider

| Provider | 说明 | 需要 Key | 推荐 |
|----------|------|---------|------|
| ahem | 自建 AHEM 服务 | 否 | 推荐 |
| yydsmail | YYDS Mail API | 是 | |
| duckmail | DuckMail API | 是 | |
| mailtm | Mail.tm | 否 | |
| tempmaillol | TempMail.lol | 否 | |

AHEM 基于 [Ad-Hoc-Email-Server](https://github.com/o4oren/Ad-Hoc-Email-Server)，无需认证，任意前缀即可收信，域名自动获取并随机选取。

## 镜像对比

| | lite | full |
|--|------|------|
| Fireworks 注册 | ✓ | ✓ |
| Grok 注册 | ✗ | ✓ |
| Turnstile solver | ✗（需外部） | ✓（内置） |
| 镜像大小 | ~150MB | ~1.5GB |
| 平台 | amd64 + arm64 | 仅 amd64 |
| 内存需求 | ~100MB | ~800MB |

## License

MIT
