# regf-works

Grok + Fireworks 多平台账号自动注册工具。纯协议/HTTP 注册，轻量快速，无需浏览器。

## 支持平台

| 平台 | 注册方式 | 产出 |
|------|---------|------|
| **Grok** | gRPC-web + Next.js Server Actions | SSO Token + 密码 + Stripe 支付链接 |
| **Fireworks** | Next.js Server Actions + AWS Cognito | API Key + 账号凭据 |

## 技术栈

- **Go 1.21+** — HTTP API 服务 + CLI 工具
- **Python 3.10+** — Fireworks 注册引擎（curl_cffi TLS 指纹）
- **Gin** — HTTP 框架
- **Viper** — 配置管理
- **Cobra** — CLI 框架

## 快速开始

### 1. 配置

```bash
cp configs/config.example.yaml configs/config.yaml
# 编辑 config.yaml，填入你的邮箱服务地址和打码服务地址
```

关键配置项：

```yaml
mail:
  provider_priority: "ahem"      # 邮箱 provider 优先级
  ahem:
    base_url: "https://your-ahem-server.com"   # AHEM 邮箱服务地址
    domains: ""                                 # 留空自动获取所有域名

turnstile:
  solver_urls:                    # Turnstile solver 地址（Grok 必需）
    - "http://your-solver:8080"
```

### 2. 运行 HTTP API 服务

```bash
# 编译运行
make build
./bin/reg-server --config configs/config.yaml

# 或直接开发模式
make dev
```

服务默认监听 `:8080`。

### 3. CLI 模式

```bash
# 注册 Grok 账号
go run cmd/cli/main.go grok --config configs/config.yaml

# 批量注册 5 个 Fireworks 账号
go run cmd/cli/main.go fireworks --config configs/config.yaml --count 5

# 指定代理
go run cmd/cli/main.go grok --proxy http://user:pass@host:port
```

### 4. Fireworks Python 服务

Fireworks 注册需要独立运行 Python 服务：

```bash
cd scripts
pip install -r requirements.txt
python fireworks_reg.py --port 5000
```

### 5. Docker 部署

```bash
docker pull ghcr.io/jiujiu532/regf-works:latest
docker run -d -p 8080:8080 -v ./configs:/app/configs ghcr.io/jiujiu532/regf-works:latest
```

## API 接口

### 注册

| 方法 | 路径 | 说明 |
|------|------|------|
| POST | `/api/grok/register` | Grok 注册（SSE 流式响应） |
| POST | `/api/fireworks/register` | Fireworks 注册（SSE 流式响应） |
| GET | `/api/health` | 健康检查 |

### 设置（运行时热更新）

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/settings/mail` | 获取邮箱服务配置 |
| POST | `/api/settings/mail` | 更新邮箱服务配置 |
| GET | `/api/settings/proxy` | 获取代理配置 |
| POST | `/api/settings/proxy` | 更新代理配置 |

### 注册请求示例

```bash
curl -N http://localhost:8080/api/grok/register \
  -H "Content-Type: application/json" \
  -d '{"proxy": "http://user:pass@host:port"}'
```

SSE 响应格式：

```
event: log
data: [*] 任务开始

event: log
data: [+] 邮箱: abc123@example.com (via ahem)

event: result
data: {"ok":true,"email":"abc123@example.com","platform":"grok","data":{"auth_token":"sso...","password":"xxx"}}
```

### 动态配置邮箱服务

```bash
curl http://localhost:8080/api/settings/mail \
  -H "Content-Type: application/json" \
  -d '{
    "provider_priority": "ahem",
    "ahem": {
      "base_url": "https://mail.example.com",
      "domains": ""
    }
  }'
```

## 项目结构

```
regf-works/
├── cmd/
│   ├── server/main.go          # HTTP API 服务入口
│   └── cli/main.go             # CLI 注册工具
├── internal/
│   ├── common/                 # 共享类型 + 工具函数
│   ├── config/                 # 配置加载
│   ├── grok/                   # Grok 注册引擎
│   ├── fireworks/              # Fireworks Go 调度层
│   └── handler/                # HTTP handlers
├── pkg/
│   ├── grpcweb/                # gRPC-web 编解码
│   ├── turnstile/              # Turnstile CAPTCHA 求解
│   └── tempmail/               # 多 provider 临时邮箱
├── scripts/
│   ├── fireworks_reg.py        # Fireworks Python 注册服务
│   └── requirements.txt
├── configs/config.example.yaml
├── Dockerfile
└── Makefile
```

## 邮箱服务

支持多种临时邮箱 provider，通过 `provider_priority` 设置优先级：

| Provider | 说明 | 需要 Key |
|----------|------|---------|
| **ahem** | 自建 AHEM 服务（推荐） | 否 |
| yydsmail | YYDS Mail API | 是 |
| duckmail | DuckMail API | 是 |
| mailtm | Mail.tm | 否 |
| tempmaillol | TempMail.lol | 否 |

AHEM 是基于 [Ad-Hoc-Email-Server](https://github.com/o4oren/Ad-Hoc-Email-Server) 的自建邮箱服务，无需认证、无需创建邮箱，任意前缀即可收信。域名从服务器自动获取，每次注册随机选取。

## 外部依赖

| 服务 | 平台 | 必需 |
|------|------|------|
| 临时邮箱服务 | 两者 | 是 |
| Turnstile solver | Grok | 是 |
| 代理 | 两者 | 推荐 |

## License

MIT
