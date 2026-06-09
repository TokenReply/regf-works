# 多阶段构建
FROM golang:1.21-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/reg-server cmd/server/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/reg-cli cmd/cli/main.go

# Python 阶段（Fireworks 注册服务）
FROM python:3.11-slim AS python-deps

WORKDIR /app/scripts
COPY scripts/requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

# 最终镜像
FROM python:3.11-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

# 从 builder 复制 Go 二进制
COPY --from=builder /bin/reg-server /app/reg-server
COPY --from=builder /bin/reg-cli /app/reg-cli

# 从 python-deps 复制 Python 依赖
COPY --from=python-deps /usr/local/lib/python3.11/site-packages /usr/local/lib/python3.11/site-packages
COPY --from=python-deps /usr/local/bin /usr/local/bin

# 复制 Python 脚本和配置模板
COPY scripts/ /app/scripts/
COPY configs/config.example.yaml /app/configs/config.example.yaml

# 默认暴露端口
EXPOSE 8080
EXPOSE 5000

# 启动脚本：同时运行 Go 服务和 Python Fireworks 服务
COPY <<'EOF' /app/entrypoint.sh
#!/bin/sh
set -e

# 如果没有配置文件，从模板复制
if [ ! -f /app/configs/config.yaml ]; then
  cp /app/configs/config.example.yaml /app/configs/config.yaml
fi

# 启动 Fireworks Python 服务（后台）
python3 /app/scripts/fireworks_reg.py --host 0.0.0.0 --port 5000 &
FIREWORKS_PID=$!

# 启动 Go HTTP 服务（前台）
exec /app/reg-server --config /app/configs/config.yaml
EOF

RUN chmod +x /app/entrypoint.sh

ENTRYPOINT ["/app/entrypoint.sh"]
