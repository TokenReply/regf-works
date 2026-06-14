#!/bin/sh
set -e

# 确保目录存在（用户可能挂载空目录）
mkdir -p /app/configs /app/data

# 如果没有用户配置文件，从模板创建
if [ ! -f /app/configs/config.yaml ]; then
  if [ -f /app/configs/config.example.yaml ]; then
    cp /app/configs/config.example.yaml /app/configs/config.yaml
    echo "[*] Created config.yaml from example template"
  elif [ -f /app/config.example.yaml.bak ]; then
    cp /app/config.example.yaml.bak /app/configs/config.yaml
    echo "[*] Created config.yaml from backup template"
  else
    echo "[WARN] No config template found, starting with defaults"
  fi
fi

# 启动 Turnstile Solver（后台）
# 线程数 / 端口 / 浏览器引擎可用环境变量覆盖（多开线程：调高 SOLVER_THREADS）
SOLVER_THREADS="${SOLVER_THREADS:-4}"
SOLVER_PORT="${SOLVER_PORT:-5072}"
SOLVER_BROWSER="${SOLVER_BROWSER:-camoufox}"
echo "[*] Starting Turnstile Solver on port ${SOLVER_PORT} (threads=${SOLVER_THREADS}, browser=${SOLVER_BROWSER})..."
python3 /app/solver/api_solver.py --browser_type "${SOLVER_BROWSER}" --thread "${SOLVER_THREADS}" --port "${SOLVER_PORT}" &
SOLVER_PID=$!

# 启动 Fireworks Python 服务（后台，端口 5000）
echo "[*] Starting Fireworks service on port 5000..."
python3 /app/scripts/fireworks_reg.py --host 0.0.0.0 --port 5000 &
FIREWORKS_PID=$!

# 启动 OpenRouter Python 服务（后台，端口 5001）
echo "[*] Starting OpenRouter service on port 5001..."
python3 /app/scripts/openrouter_reg.py --host 0.0.0.0 --port 5001 &
OPENROUTER_PID=$!

# 启动 Novita Python 服务（后台，端口 5002）
echo "[*] Starting Novita service on port 5002..."
python3 /app/scripts/novita_reg.py --host 0.0.0.0 --port 5002 &
NOVITA_PID=$!

# 启动 Outlook 收件服务（后台，端口 5003）—— IMAP XOAUTH2，给 Go(grok) 调用
echo "[*] Starting Outlook mail service on port 5003..."
python3 /app/scripts/outlook_mail.py --host 0.0.0.0 --port 5003 &
OUTLOOK_PID=$!

# 等待服务就绪
echo "[*] Waiting for services to be ready..."
sleep 5

# 启动 Go HTTP 服务（前台）
echo "[*] Starting main server on port 8080..."
exec /app/reg-server --config /app/configs/config.yaml
