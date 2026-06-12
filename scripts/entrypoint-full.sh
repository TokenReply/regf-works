#!/bin/sh
set -e

# 如果没有用户配置文件，使用 full 默认配置
if [ ! -f /app/configs/config.yaml ]; then
  cp /app/configs/config.full.yaml /app/configs/config.yaml
fi

# 启动 Turnstile Solver（后台，端口 5072，2 线程）
echo "[*] Starting Turnstile Solver on port 5072..."
python3 /app/solver/api_solver.py --browser_type camoufox --thread 2 --port 5072 --headless &
SOLVER_PID=$!

# 启动 Fireworks Python 服务（后台，端口 5000）
echo "[*] Starting Fireworks service on port 5000..."
python3 /app/scripts/fireworks_reg.py --host 0.0.0.0 --port 5000 &
FIREWORKS_PID=$!

# 启动 OpenRouter Python 服务（后台，端口 5001）
echo "[*] Starting OpenRouter service on port 5001..."
python3 /app/scripts/openrouter_reg.py --host 0.0.0.0 --port 5001 &
OPENROUTER_PID=$!

# 等待服务就绪
echo "[*] Waiting for services to be ready..."
sleep 5

# 启动 Go HTTP 服务（前台）
echo "[*] Starting main server on port 8080..."
exec /app/reg-server --config /app/configs/config.yaml
