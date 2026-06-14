# 一体化 Dockerfile — Grok + Fireworks + OpenRouter + Novita + Turnstile Solver
# 构建: docker build -t regf-works:latest .
# 运行: docker run -p 8080:8080 --name regf-works regf-works:latest

# ========== 阶段 1: 编译 Go 二进制 ==========
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/reg-server cmd/server/main.go

# ========== 阶段 2: 最终镜像 ==========
FROM python:3.11-slim

# 安装系统依赖（Camoufox 浏览器需要）
RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    libgtk-3-0 \
    libdbus-glib-1-2 \
    libxt6 \
    libx11-xcb1 \
    libasound2 \
    libdrm2 \
    libgbm1 \
    libxrandr2 \
    libxcomposite1 \
    libxdamage1 \
    libxfixes3 \
    libpango-1.0-0 \
    libcairo2 \
    libatk1.0-0 \
    libatk-bridge2.0-0 \
    libxkbcommon0 \
    libcups2 \
    libxshmfence1 \
    fonts-liberation \
    xvfb \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 创建数据和配置目录
RUN mkdir -p /app/data /app/configs

# 复制 Go 二进制
COPY --from=builder /bin/reg-server /app/reg-server

# 复制配置文件（主位置 + 备份，防止挂载整个 configs 目录时消失）
COPY configs/config.example.yaml /app/configs/config.example.yaml
COPY configs/config.example.yaml /app/config.example.yaml.bak

# 复制 Python 脚本
COPY scripts/fireworks_reg.py /app/scripts/fireworks_reg.py
COPY scripts/openrouter_reg.py /app/scripts/openrouter_reg.py
COPY scripts/novita_reg.py /app/scripts/novita_reg.py
COPY scripts/outlook_mail.py /app/scripts/outlook_mail.py
COPY solver/ /app/solver/

# 复制前端资源
COPY web/ /app/web/

# 安装 Python 依赖
COPY scripts/requirements.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt

# 安装 Camoufox 浏览器（重试 3 次避免 GitHub API 限流）
RUN for i in 1 2 3; do python -m camoufox fetch && break || sleep 10; done

# 安装 Patchright 浏览器（备用）
RUN python -m patchright install chromium || true

# 暴露端口
EXPOSE 8080

# 复制启动脚本
COPY scripts/entrypoint.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

# 设置环境变量
ENV PYTHONUNBUFFERED=1

ENTRYPOINT ["/app/entrypoint.sh"]
