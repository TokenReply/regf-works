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

# 启动 Turnstile Solver（后台，线程数可通过 SOLVER_THREADS 环境变量配置，默认 2）
SOLVER_THREADS=${SOLVER_THREADS:-2}
echo "[*] Starting Turnstile Solver on port 5072 (threads=$SOLVER_THREADS)..."
python3 /app/solver/api_solver.py --browser_type camoufox --thread $SOLVER_THREADS --port 5072 &
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

# 等待服务就绪
echo "[*] Waiting for services to be ready..."
sleep 5

# 启动 Go HTTP 服务（前台）
echo "[*] Starting main server on port 8080..."
exec /app/reg-server --config /app/configs/config.yaml
