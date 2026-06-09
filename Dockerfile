# 默认 Dockerfile — 构建 full 版本（Fireworks + Grok + Solver）
# 如需仅构建 Fireworks 轻量版，使用: docker build -f Dockerfile.lite .

FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/reg-server cmd/server/main.go
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /bin/reg-cli cmd/cli/main.go

# 最终镜像
FROM python:3.11-slim

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
    fonts-liberation \
    xvfb \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /app

COPY --from=builder /bin/reg-server /app/reg-server
COPY --from=builder /bin/reg-cli /app/reg-cli

COPY scripts/requirements-full.txt /tmp/requirements.txt
RUN pip install --no-cache-dir -r /tmp/requirements.txt && rm /tmp/requirements.txt

RUN python -m camoufox fetch

COPY scripts/fireworks_reg.py /app/scripts/fireworks_reg.py
COPY scripts/turnstile_solver.py /app/scripts/turnstile_solver.py
COPY configs/config.example.yaml /app/configs/config.example.yaml
COPY configs/config.full.yaml /app/configs/config.full.yaml

EXPOSE 8080 5000 8888

COPY scripts/entrypoint-full.sh /app/entrypoint.sh
RUN chmod +x /app/entrypoint.sh

ENTRYPOINT ["/app/entrypoint.sh"]
