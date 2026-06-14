# regf-works

**Grok / Fireworks / OpenRouter / Novita AI 自动注册平台**

批量自动注册 + 验证码打码 + 邮箱验证 + API Key 创建，一站式完成。

## 支持平台

| 平台 | 注册 | 验证 | API Key | 备注 |
|------|:----:|:----:|:-------:|------|
| Grok | ✅ | Turnstile + 邮箱验证码 | auth_token | NSFW/Unhinged 自动开启 |
| Fireworks AI | ✅ | 邮箱验证码 | API Key | Server Action 模式 |
| OpenRouter | ✅ | Turnstile + 邮箱验证链接 | API Key | Clerk 认证 |
| Novita AI | ✅ | Turnstile + 邮箱激活 | API Key | 自动填问卷获取 $101 额度 |

## 功能特性

- **后台任务队列**：关闭浏览器任务继续运行，重新打开可查看进度
- **多邮箱 Provider**：AHEM、YYDS Mail、GPTMail（自动获取公共密钥）、MoeMail
- **代理池支持**：HTTP / SOCKS5 代理轮换，每个任务固定 IP
- **Turnstile 打码**：Camoufox 浏览器引擎，支持 per-request proxy
- **域名黑名单**：自动拉黑被拒域名，永久生效
- **配置持久化**：Web UI 修改的配置自动保存，容器重启不丢
- **中英文切换**：无刷新语言切换
- **结果管理**：复选框批量删除、筛选删除、多格式导出

---

## 快速启动

### Docker 部署

```bash
docker run -d \
  --name regf-works \
  -p 8080:8080 \
  -v ./configs:/app/configs \
  -v ./data:/app/data \
  -e AUTH_PASSWORD=yourpassword \
  -e AHEM_BASE_URL=https://mail.example.com \
  ghcr.io/jiujiu532/regf-works:latest
```

访问 `http://localhost:8080`，默认账号 `admin`。

### Docker Compose（推荐）

```yaml
services:
  regf-works:
    image: ghcr.io/jiujiu532/regf-works:latest
    container_name: regf-works
    restart: unless-stopped
    ports:
      - "8080:8080"
    environment:
      - AUTH_PASSWORD=yourpassword
      - AHEM_BASE_URL=https://mail.example.com
      - PROXY_DEFAULT=http://proxy:8118
    volumes:
      - ./configs:/app/configs
      - ./data:/app/data
```

> **必须挂载 `configs` 和 `data` 两个卷**，否则容器重启后配置和注册结果全部丢失。

### 搭配 WARP 代理（防风控）

```yaml
services:
  regf-works:
    image: ghcr.io/jiujiu532/regf-works:latest
    ports:
      - "8080:8080"
    environment:
      - PROXY_DEFAULT=http://privoxy:8118
    volumes:
      - ./configs:/app/configs
      - ./data:/app/data
    networks:
      - warp-net

  warp-proxy:
    image: caomingjun/warp
    cap_add: [NET_ADMIN, SYS_MODULE]
    networks:
      - warp-net

  privoxy:
    image: vimagick/privoxy
    networks:
      - warp-net

networks:
  warp-net:
```

代理池配置（Settings → 代理池）：
```
socks5://warp-proxy:1080
socks5://warp-proxy-2:1080
socks5://warp-proxy-3:1080
```

### 本地开发（Windows）

```bash
# 1. 启动打码服务
cd solver
python api_solver.py --browser_type camoufox --thread 2 --port 5072

# 2. 启动主服务
start.bat
```

---

## 架构

```
┌──────────────────────────────────────────────────────┐
│                  Web UI (:8080)                       │
│        注册 │ 结果 │ 邮箱 │ 设置                       │
└──────────┬───────────────────────────────────────────┘
           │ REST API + SSE
┌──────────▼───────────────────────────────────────────┐
│              Go 主服务 (Gin)                           │
│  TaskManager → 后台任务队列（关浏览器不停）              │
│  ResultStorage → 结果持久化 (data/results.json)        │
│  BlacklistManager → 域名黑名单 (data/blacklist.json)   │
│  ConfigManager → 配置持久化 (configs/config.yaml)      │
└─┬──────────┬──────────┬──────────┬───────────────────┘
  │          │          │          │
  ▼          ▼          ▼          ▼
Fireworks  OpenRouter  Novita   Turnstile
Python     Python      Python   Solver
:5000      :5001       :5002    :5072 (Camoufox)
```

---

## API

### 后台任务（推荐）

```bash
# 提交任务（关浏览器继续跑）
POST /api/tasks
{"platform":"novita","count":10,"concurrency":1,"delay":60}
# → {"id":"550e8400-...","status":"running"}

# 查看所有任务
GET /api/tasks

# 订阅任务日志（SSE，支持断点续传）
GET /api/tasks/:id/logs?offset=0

# 取消任务
DELETE /api/tasks/:id
```

### 传统注册（SSE 流式，兼容）

```bash
POST /api/grok/register
POST /api/fireworks/register
POST /api/openrouter/register
POST /api/novita/register
```

### 其他

```bash
GET  /api/health
GET  /api/results
DELETE /api/results/batch      # {"indices":[0,2,5]}
DELETE /api/results/filter     # {"platform":"grok","status":"failed"}
GET  /api/blacklist/{platform}
DELETE /api/blacklist/{platform}
GET  /api/settings/mail
POST /api/settings/mail
GET  /api/settings/proxy
POST /api/settings/proxy
```

---

## 环境变量

| 变量 | 说明 | 默认值 |
|------|------|--------|
| `AUTH_USERNAME` | 管理员用户名 | admin |
| `AUTH_PASSWORD` | 管理员密码 | admin123 |
| `AUTH_JWT_SECRET` | JWT 签名密钥 | change-me |
| `PROXY_DEFAULT` | 默认代理 | - |
| `AHEM_BASE_URL` | AHEM 邮箱服务地址 | - |
| `AHEM_DOMAINS` | AHEM 域名列表 | 自动获取 |
| `YYDS_BASE_URL` | YYDS 邮箱地址 | - |
| `YYDS_API_KEY` | YYDS API Key | - |
| `GPTMAIL_BASE_URL` | GPTMail 地址 | https://mail.chatgpt.org.uk |
| `GPTMAIL_API_KEY` | GPTMail Key（留空自动获取公共密钥） | 自动 |
| `MOEMAIL_BASE_URL` | MoeMail 地址 | - |
| `MOEMAIL_API_KEY` | MoeMail Key | - |
| `MAIL_PROVIDER_PRIORITY` | Provider 优先级 | ahem |
| `TURNSTILE_SOLVER_PROXY` | Solver 代理 | - |
| `GROK_SITE_KEY` | Grok Turnstile sitekey | 自动扫描 |
| `GROK_ACTION_ID` | Grok Server Action ID | 自动扫描 |
| `FIREWORKS_SERVICE_URL` | Fireworks Python 服务 | 127.0.0.1:5000 |
| `OPENROUTER_SERVICE_URL` | OpenRouter Python 服务 | 127.0.0.1:5001 |
| `NOVITA_SERVICE_URL` | Novita Python 服务 | 127.0.0.1:5002 |

---

## 项目结构

```
.
├── cmd/server/main.go       # Go 服务入口
├── internal/
│   ├── task/                # 后台任务队列
│   ├── grok/                # Grok 注册引擎
│   ├── fireworks/           # Fireworks 注册引擎
│   ├── openrouter/          # OpenRouter 注册引擎
│   ├── novita/              # Novita 注册引擎
│   ├── handler/             # HTTP 处理器
│   ├── config/              # 配置管理（环境变量+YAML+持久化）
│   └── common/              # 公共工具（代理/存储/日志）
├── pkg/
│   ├── tempmail/            # 邮箱 Provider（AHEM/YYDS/GPTMail/MoeMail）
│   ├── turnstile/           # Turnstile 验证码求解器
│   └── grpcweb/             # gRPC-web 编码
├── scripts/
│   ├── fireworks_reg.py     # Fireworks Python 服务 (:5000)
│   ├── openrouter_reg.py    # OpenRouter Python 服务 (:5001)
│   ├── novita_reg.py        # Novita Python 服务 (:5002)
│   └── entrypoint.sh        # Docker 启动脚本
├── solver/                  # Turnstile Solver (Camoufox :5072)
├── web/index.html           # 前端单文件 SPA
├── Dockerfile               # 一体化镜像
├── configs/config.example.yaml
└── start.bat                # Windows 启动脚本
```

---

## 资源需求

| 项目 | 最低配置 |
|------|---------|
| CPU | 2 核 |
| 内存 | 2 GB |
| 磁盘 | 5 GB |
| 镜像大小 | ~2.5 GB |

> Camoufox 浏览器引擎需要较多资源，主要用于 Turnstile 验证码求解。

---

## 注意事项

1. **生产部署必须修改默认密码**（`AUTH_PASSWORD` 环境变量）
2. **必须挂载 configs 和 data 卷**（否则重启丢配置）
3. **至少配置一个邮箱服务**（AHEM 推荐自建）
4. **合理设置注册间隔**（Novita 建议 60s+，避免风控）
5. **代理池 IP 需一致**（打码和注册必须同一出口 IP）

---

## License

MIT
