@echo off
chcp 65001 >nul 2>&1
title regf-works

cd /d "D:\Python\fireworks-re\grok-fireworks-reg"

echo ========================================
echo   regf-works
echo ========================================
echo.
echo Please start Turnstile Solver first:
echo   D:\Python\openrouter rot\openrouter\启动打码服务.bat
echo   Port: 5072
echo.
echo Press any key to continue...
pause >nul

start "FW-Python" python scripts\fireworks_reg.py --host 0.0.0.0 --port 5000
start "OR-Python" python scripts\openrouter_reg.py --host 0.0.0.0 --port 5001
start "NV-Python" python scripts\novita_reg.py --host 0.0.0.0 --port 5002

timeout /t 3 /nobreak >nul

REM 设置代理环境变量（让 Go 服务走 Clash）
set HTTP_PROXY=http://127.0.0.1:7890
set HTTPS_PROXY=http://127.0.0.1:7890
set NO_PROXY=127.0.0.1,localhost

echo.
echo ========================================
echo Turnstile Solver:   http://localhost:5072
echo Fireworks Service:  http://localhost:5000
echo OpenRouter Service: http://localhost:5001
echo Novita Service:     http://localhost:5002
echo Web UI:             http://127.0.0.1:8080
echo Login:              admin / admin123
echo ========================================
echo.

bin\reg-server.exe --config configs\config.yaml

pause
