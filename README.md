# regf-works

🚀 **Grok / Fireworks / OpenRouter 自动注册平台**

一体化自动化注册服务，支持 Grok、Fireworks AI 和 OpenRouter 三个平台的批量注册。

## ✨ 特性

- 🎯 **三平台支持**：Grok、Fireworks AI、OpenRouter
- 🌐 **Web UI**：现代化管理界面，实时日志流
- 🔐 **多邮箱 Provider**：AHEM、YYDS Mail、GPTMail、MoeMail
- 🤖 **自动打码**：集成 Turnstile Solver（Camoufox 引擎）
- 🐳 **Docker 一体化部署**：开箱即用
- 📊 **黑名单管理**：自动过滤被拒域名
- 🔄 **并发控制**：可配置并发数
- 📝 **导出功能**：JSON/TXT/纯凭证格式

---

## 🚀 快速启动

### Docker 部署（推荐）

```bash
# 构建镜像
docker build -t regf-works .

# 运行容器
docker run -d \
  --name regf-works \
  -p 8080:8080 \
  -e AUTH_PASSWORD=your_password \
  regf-works

# 访问
open http://localhost:8080
```

### 本地开发

**前置要求**：
- Go 1.23+
- Python 3.11+
- 已安装 Camoufox（`python -m camoufox fetch`）

**启动**：
```bash
# 1. 启动 Turnstile Solver（5072 端口）
cd solver
python api_solver.py --browser_type camoufox --thread 2 --port 5072

# 2. 启动主服务（新终端）
cd grok-fireworks-reg
start.bat  # Windows
# 或 ./start.sh（Linux/macOS，需自行创建）
```

---

## 📖 架构设计

```
┌─────────────────────────────────────────────────┐
│              Web UI (Vue.js)                    │
│           http://localhost:8080                 │
└────────────────┬────────────────────────────────┘
                 │ REST API (SSE)
┌────────────────▼────────────────────────────────┐
│           Go 主服务 (Gin)                        │
│   • API 路由                                     │
│   • 认证/授权                                    │
│   • 任务调度                                     │
│   • 黑名单管理                                   │
└─┬───────────────────┬───────────────────────┬───┘
  │                   │                       │
  │ HTTP              │ HTTP                  │ HTTP
  ▼                   ▼                       ▼
┌─────────────┐ ┌──────────────┐ ┌────────────────┐
│  Fireworks  │ │  OpenRouter  │ │ Turnstile      │
│  Python     │ │  Python      │ │ Solver         │
│  Service    │ │  Service     │ │ (Camoufox)     │
│  :5000      │ │  :5001       │ │ :5072          │
└─────────────┘ └──────────────┘ └────────────────┘
      │                 │                 ▲
      │                 │                 │
      └─────────────────┴─────────────────┘
              共享打码服务
```

---

## 🛠️ 配置说明

### 核心配置文件：`configs/config.yaml`

```yaml
server:
  port: 8080

auth:
  username: "admin"
  password: "admin123"  # ⚠️ 生产环境请修改

mail:
  provider_priority: "ahem"  # ahem, yydsmail, gptmail, moemail
  ahem:
    base_url: "https://mail.jiuuij.de5.net"
    domains: ""

turnstile:
  solver_urls: ["http://localhost:5072"]

grok:
  site_key: "0x4AAAAAAAhr9JGVDZbrZOo0"

fireworks:
  service_url: "http://127.0.0.1:5000"
  max_concurrent: 10

openrouter:
  service_url: "http://127.0.0.1:5001"
  max_concurrent: 10
  solver_type: "selfhost"
  solver_api: "http://localhost:5072"
```

### 环境变量（Docker）

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `AUTH_USERNAME` | 管理员用户名 | admin |
| `AUTH_PASSWORD` | 管理员密码 | admin123 |
| `AHEM_BASE_URL` | AHEM 邮箱服务 | - |
| `YYDS_BASE_URL` | YYDS 邮箱服务 | - |
| `YYDS_API_KEY` | YYDS API Key | - |
| `PROXY_DEFAULT` | 默认代理 | - |

---

## 📚 API 文档

### 健康检查
```bash
GET /api/health
```

### 注册接口（SSE 流式）
```bash
POST /api/grok/register
POST /api/fireworks/register
POST /api/openrouter/register

请求体：
{
  "count": 5,
  "concurrency": 2,
  "proxy": "http://proxy:port",
  "email_provider": "ahem"
}
```

### 黑名单管理
```bash
GET /api/blacklist/grok
GET /api/blacklist/fireworks
GET /api/blacklist/openrouter

DELETE /api/blacklist/grok
DELETE /api/blacklist/fireworks
DELETE /api/blacklist/openrouter
```

---

## 🐳 Docker 详细部署

详见 [docs/DOCKER.md](docs/DOCKER.md)

**资源需求**：
- CPU：2 核心
- 内存：2GB
- 磁盘：5GB
- 镜像大小：约 2.5GB

---

## 🔧 开发指南

### 项目结构
```
.
├── cmd/server/          # Go 主服务入口
├── internal/            # Go 核心逻辑
│   ├── grok/           # Grok 注册引擎
│   ├── fireworks/      # Fireworks 注册引擎
│   ├── openrouter/     # OpenRouter 注册引擎
│   ├── handler/        # HTTP 处理器
│   └── config/         # 配置管理
├── scripts/            # Python 注册服务
│   ├── fireworks_reg.py
│   └── openrouter_reg.py
├── solver/             # Turnstile 打码服务
│   ├── api_solver.py
│   └── browser_configs.py
├── web/                # 前端静态资源
│   └── index.html
├── Dockerfile          # 一体化镜像
└── configs/
    ├── config.example.yaml
    └── config.full.yaml
```

### 构建
```bash
# Go 服务
go build -o bin/reg-server cmd/server/main.go

# Docker 镜像
docker build -t regf-works .
```

---

## 📊 技术栈

**后端**：
- Go 1.23 + Gin
- Python 3.11 + Quart
- Camoufox (浏览器引擎)
- Patchright (备用浏览器)

**前端**：
- 原生 JavaScript + CSS
- SSE (Server-Sent Events)

**部署**：
- Docker + Multi-stage Build
- Debian Slim 基础镜像

---

## ⚠️ 注意事项

1. **生产部署必须修改默认密码**
2. **配置邮箱服务**（AHEM/YYDS/GPTMail/MoeMail 至少一个）
3. **Solver 需要 2 核 CPU + 2GB 内存**（浏览器渲染）
4. **合理设置并发数**（避免 IP 封禁）
5. **不要滥用注册服务**（遵守各平台 ToS）

---

## 📄 License

MIT License

---

## 🙏 致谢

- [Camoufox](https://github.com/daijro/camoufox) - 反检测浏览器引擎
- [Patchright](https://github.com/Vinyzu/patchright) - Playwright 补丁版本
- [Gin](https://github.com/gin-gonic/gin) - Go Web 框架
- [Quart](https://github.com/pallets/quart) - Python 异步 Web 框架

---

## 📮 联系方式

- Issues: [GitHub Issues](https://github.com/jiujiu532/regf-works/issues)
- 文档: [docs/](docs/)

---

**⚡ 开箱即用，一键部署，享受自动化注册的便利！**
