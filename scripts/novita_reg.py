"""
Novita AI 全自动注册服务（Quart HTTP 端点）。

API:
  POST /novita/process  { email, password?, proxy?, solver_api?,
                          mail_provider?, mail_meta?, ahem_base_url? }

流式响应：每行 LOG:xxx 为实时日志，最后一行为 JSON 注册结果。

Usage: python novita_reg.py [--host 0.0.0.0] [--port 5002]
"""

import argparse
import asyncio
import json
import logging
import os
import queue
import random
import re
import secrets
import string
import time
import ssl
import urllib.request
import urllib.error
import urllib.parse
from threading import Event
from typing import Optional

import httpx
from quart import Quart, request, jsonify

try:
    from outlook_mail import poll_outlook_async  # Outlook 账号池 IMAP XOAUTH2 收件
except Exception:  # pragma: no cover
    poll_outlook_async = None

# ─── 日志 ─────────────────────────────────────────────────────────────────
logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("novita-reg")

# ─── 常量 ─────────────────────────────────────────────────────────────────
NOVITA_API = "https://api-server.novita.ai"
NOVITA_ORIGIN = "https://novita.ai"
TURNSTILE_SITEKEY = "0x4AAAAAAAaG28VfN_OxkED8"
TURNSTILE_PAGE_URL = "https://novita.ai/user/register"

NOVITA_HEADERS = {
    "Content-Type": "application/json",
    "Origin": NOVITA_ORIGIN,
    "Referer": f"{NOVITA_ORIGIN}/user/register",
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/125.0.0.0 Safari/537.36"
    ),
}

# 问卷随机数据
ROLES = ["developer", "data-scientist", "researcher", "student", "product-manager"]
SPENDS = ["0-100", "100-500", "500-1000"]
FIRST_NAMES = ["Alex", "Sam", "Jordan", "Morgan", "Casey", "Riley", "Quinn", "Avery"]
LAST_NAMES = ["Chen", "Wang", "Li", "Zhang", "Liu", "Yang", "Huang", "Wu", "Zhou", "Lin"]
COMPANIES = ["TechFlow AI", "DataVerse", "CloudMind", "NeuralHub", "AIForge", "DeepCore",
             "QuantumAI", "SynthLab", "CogniTech", "ByteWave"]

# ─── Quart App ────────────────────────────────────────────────────────────
app = Quart(__name__)
_MAX_CONCURRENT = int(os.environ.get("NOVITA_MAX_CONCURRENT", "5"))
_novita_semaphore: asyncio.Semaphore


# ─── Turnstile Solver ─────────────────────────────────────────────────────

async def solve_turnstile(solver_api: str, logf, proxy: str = "") -> Optional[str]:
    """调用本地 Solver 获取 Turnstile token"""
    logf("[*] 提交 Turnstile 验证...")

    params = {"url": TURNSTILE_PAGE_URL, "sitekey": TURNSTILE_SITEKEY}
    if proxy:
        params["proxy"] = proxy

    async with httpx.AsyncClient(timeout=15) as client:
        try:
            r = await client.get(f"{solver_api}/turnstile", params=params)
            data = r.json()
        except Exception as e:
            logf(f"[-] Solver 连接失败: {e}")
            return None

        if data.get("errorId") != 0:
            logf(f"[-] Solver 任务提交失败: {data}")
            return None

        task_id = data["taskId"]
        logf(f"[*] 任务已提交: {task_id[:8]}...")

        for i in range(60):
            await asyncio.sleep(2)
            try:
                r = await client.get(f"{solver_api}/result", params={"id": task_id})
                result = r.json()
            except Exception as e:
                logf(f"[-] 轮询失败: {e}")
                continue

            if result.get("status") == "processing":
                if i % 5 == 0:
                    logf(f"[*] 等待打码中... ({i * 2}s)")
                continue

            if result.get("errorId") == 0 and result.get("status") == "ready":
                token = result["solution"]["token"]
                logf(f"[+] Turnstile 验证通过!")
                return token

            if result.get("errorCode") == "ERROR_CAPTCHA_UNSOLVABLE":
                logf("[-] Solver 无法解决验证码")
                return None

            logf(f"[-] Solver 未知响应: {result}")
            return None

    logf("[-] Turnstile 超时 (120s)")
    return None


# ─── 注册流程 ─────────────────────────────────────────────────────────────

async def _do_novita_register(
    email: str,
    password: str,
    proxy: str,
    solver_api: str,
    mail_provider: str,
    mail_meta: dict,
    ahem_base_url: str,
    yydsmail_url: str,
    yydsmail_key: str,
    log_q: queue.Queue,
    cancel: Event,
) -> dict:
    """执行完整的 Novita AI 注册流程"""

    def logf(msg: str):
        log_q.put(msg)
        logger.info(msg)

    result = {"ok": False, "email": email, "password": password, "api_key": "", "error": ""}

    # 创建一个共享 session 的 client（保持 Cloudflare cookie）
    transport = httpx.AsyncHTTPTransport(proxy=proxy) if proxy else None
    async with httpx.AsyncClient(
        timeout=30,
        transport=transport,
        headers={
            "User-Agent": NOVITA_HEADERS["User-Agent"],
            "Accept-Language": "en-US,en;q=0.9",
        },
        follow_redirects=True,
    ) as client:

      try:
        # ──── Step 0: 预热获取 Cloudflare cookie ────
        logf("[*] 开始 Novita 注册流程")
        if proxy:
            logf(f"[*] 使用代理: {proxy[:30]}...")
        await client.get("https://novita.ai/user/register")

        # ──── Step 1: 注册 ────
        cf_token = await solve_turnstile(solver_api, logf, proxy)
        if not cf_token:
            result["error"] = "Turnstile 打码失败 (注册)"
            return result

        logf(f"[*] 注册中... {email}")
        payload = {
            "email": email,
            "password": password,
            "confirmPassword": password,
            "redirectUrl": "/user/login",
            "cloudflareToken": cf_token,
            "allowNotification": True,
            "fromInviteCode": "",
        }

        r = await client.post(
            f"{NOVITA_API}/v1/user/register",
            json=payload,
            headers=NOVITA_HEADERS,
        )

        if r.status_code != 200:
            err_data = r.json() if r.headers.get("content-type", "").startswith("application/json") else {}
            reason = err_data.get("reason", "")
            message = err_data.get("message", r.text[:200])
            logf(f"[-] 注册失败: HTTP {r.status_code} {reason} {message}")
            result["error"] = f"{reason}: {message}"
            result["retriable"] = reason not in ("USER_ALREADY_EXISTS", "EMAIL_ILLEGAL_ERROR")
            return result

        logf("[+] 注册成功!")

        if cancel.is_set():
            result["error"] = "任务取消"
            return result

        # ──── Step 2: 等待激活邮件 ────
        logf("[*] 等待激活邮件...")
        activate_token = await _wait_for_activation_email(
            email, mail_provider, mail_meta, ahem_base_url,
            yydsmail_url, yydsmail_key, logf
        )
        if not activate_token:
            result["error"] = "获取激活邮件失败"
            result["ok"] = False
            logf("[!] 注册已成功但激活失败，账号需手动激活")
            return result

        if cancel.is_set():
            result["error"] = "任务取消"
            return result

        # ──── Step 3: 激活 ────
        logf("[*] 激活账户...")
        cf_token2 = await solve_turnstile(solver_api, logf, proxy)
        if not cf_token2:
            result["error"] = "Turnstile 打码失败 (激活)"
            logf("[!] 注册已成功但激活打码失败")
            return result

        r = await client.post(
            f"{NOVITA_API}/v1/user/email/verify",
            json={
                "token": activate_token,
                "email": email,
                "cloudflareToken": cf_token2,
            },
            headers=NOVITA_HEADERS,
        )

        if r.status_code != 200:
            logf(f"[-] 激活失败: HTTP {r.status_code} {r.text[:200]}")
            result["error"] = f"激活失败: {r.text[:200]}"
            return result

        logf("[+] 账户已激活!")

        if cancel.is_set():
            result["error"] = "任务取消"
            return result

        # ──── Step 4: 登录 ────
        logf("[*] 登录获取 JWT...")
        cf_token3 = await solve_turnstile(solver_api, logf, proxy)
        if not cf_token3:
            result["error"] = "Turnstile 打码失败 (登录)"
            return result

        r = await client.post(
            f"{NOVITA_API}/v1/user/login",
            json={
                "email": email,
                "password": password,
                "cloudflareToken": cf_token3,
            },
            headers=NOVITA_HEADERS,
        )

        if r.status_code != 200:
            logf(f"[-] 登录失败: HTTP {r.status_code} {r.text[:200]}")
            result["error"] = f"登录失败: {r.text[:200]}"
            return result

        jwt = r.json().get("token", "")
        if not jwt:
            logf("[-] 登录响应中无 token")
            result["error"] = "登录响应中无 token"
            return result

        logf("[+] 登录成功!")
        auth_headers = {
            "Content-Type": "application/json",
            "Authorization": f"Bearer {jwt}",
        }

        # ──── Step 5: 问卷 ────
        logf("[*] 提交问卷获取额度...")
        questionnaire = {
            "name": f"{random.choice(FIRST_NAMES)} {random.choice(LAST_NAMES)}",
            "company": random.choice(COMPANIES),
            "role": random.choice(ROLES),
            "monthlySpend": random.choice(SPENDS),
        }

        r = await client.post(
            f"{NOVITA_API}/v1/user/questionnaire",
            json=questionnaire,
            headers=auth_headers,
        )

        if r.status_code == 200:
            logf("[+] 问卷提交成功 ($101 额度)")
        else:
            logf(f"[!] 问卷提交失败: {r.status_code} (不影响账户)")

        # ──── Step 6: 创建 API Key ────
        logf("[*] 创建 API Key...")
        r = await client.post(
            f"{NOVITA_API}/v2/user/key",
            json={"name": "auto-generated"},
            headers=auth_headers,
        )

        if r.status_code != 200:
            logf(f"[-] 创建 API Key 失败: {r.status_code}")
            result["error"] = f"创建 API Key 失败: {r.text[:200]}"
            return result

        key_data = r.json()
        api_key = key_data.get("apiKey", "")
        logf(f"[+] API Key: {api_key[:15]}...")

        result["ok"] = True
        result["api_key"] = api_key
        logf(f"[OK] 注册完成: {email}")
        return result

      except Exception as e:
        logf(f"[-] 异常: {e}")
        result["error"] = str(e)
        return result


async def _wait_for_activation_email(
    email: str, mail_provider: str, mail_meta: dict,
    ahem_base_url: str, yydsmail_url: str, yydsmail_key: str, logf
) -> Optional[str]:
    """从邮箱中获取激活 token（支持 AHEM / YYDS / GPTMail）"""

    if mail_provider == "ahem":
        return await _poll_ahem_activation(email, ahem_base_url, logf)
    elif mail_provider in ("yydsmail", "yyds"):
        return await _poll_yydsmail_activation(email, yydsmail_url, yydsmail_key, mail_meta, logf)
    elif mail_provider == "duckmail":
        return await _poll_duckmail_activation(email, mail_meta, logf)
    elif mail_provider == "outlook":
        return await _poll_outlook_activation(email, mail_meta, logf)
    elif mail_provider in ("gptmail", "moemail"):
        # GPTMail/MoeMail 都走通用 AHEM 兼容接口
        return await _poll_ahem_activation(email, ahem_base_url, logf)
    else:
        # 默认尝试 AHEM
        if ahem_base_url:
            return await _poll_ahem_activation(email, ahem_base_url, logf)
        logf("[-] 无可用邮箱服务地址")
        return None


async def _poll_ahem_activation(email: str, ahem_base_url: str, logf) -> Optional[str]:
    """从 AHEM 邮箱提取 Novita 激活 token"""
    if not ahem_base_url:
        logf("[-] 无 AHEM 服务地址")
        return None

    prefix = email.split("@")[0]
    for attempt in range(15):
        await asyncio.sleep(2)
        try:
            async with httpx.AsyncClient(timeout=10) as client:
                r = await client.get(f"{ahem_base_url}/api/mailbox/{prefix}/email")
                if r.status_code != 200:
                    continue
                mails = r.json()
                if not mails:
                    if attempt % 3 == 0:
                        logf(f"[*] 等待激活邮件... ({attempt * 2}s)")
                    continue

                for mail in mails:
                    if "novita" in mail.get("sender", {}).get("address", "").lower() or \
                       "confirm" in mail.get("subject", "").lower():
                        email_id = mail.get("emailId", "")
                        r2 = await client.get(f"{ahem_base_url}/api/mailbox/{prefix}/email/{email_id}")
                        html = r2.json().get("html", "")
                        token = _extract_novita_token(html)
                        if token:
                            logf("[+] 激活 token 已获取")
                            return token
        except Exception as e:
            if attempt % 3 == 0:
                logf(f"[*] 获取邮件出错: {e}, 重试中...")
    logf("[-] 激活邮件超时 (30s)")
    return None


async def _poll_yydsmail_activation(
    email: str, yydsmail_url: str, yydsmail_key: str, mail_meta: dict, logf
) -> Optional[str]:
    """从 YYDS Mail 提取 Novita 激活 token

    对齐 Go 端 yydsmail.go 的真实 API（之前用 /api/emails + API key 当 Bearer 导致 403）：
      GET {base}/v1/messages?address=<email>   Bearer=<创建邮箱时的 per-inbox token>
      GET {base}/v1/messages/{id}              Bearer=<同上>
    base 统一收敛到根域名（配置里可能带 /v1），收件凭证优先用 meta['token']。
    """
    # per-inbox token 优先；缺失再退回 API key（多数情况下 API key 对 messages 无权 → 403）
    token_header = mail_meta.get("token") or mail_meta.get("yydsmail_token") or yydsmail_key
    if not token_header:
        logf("[-] 无 YYDS Mail token/key")
        return None

    _ssl_ctx = ssl.create_default_context()
    _ssl_ctx.check_hostname = False
    _ssl_ctx.verify_mode = ssl.CERT_NONE

    base_url = (yydsmail_url or "https://maliapi.215.im").strip().rstrip("/")
    if base_url.endswith("/v1"):
        base_url = base_url[:-3].rstrip("/")
    if not base_url.startswith("http"):
        base_url = "https://" + base_url

    def _get(url):
        req = urllib.request.Request(url, headers={
            "Authorization": f"Bearer {token_header}",
            "Accept": "application/json",
            "User-Agent": "Mozilla/5.0 (compatible; RegPlatform/1.0)",
        })
        with urllib.request.urlopen(req, context=_ssl_ctx, timeout=10) as resp:
            return json.loads(resp.read())

    list_url = f"{base_url}/v1/messages?address={urllib.parse.quote(email)}"
    for attempt in range(15):
        await asyncio.sleep(2)
        try:
            data = _get(list_url)
            if isinstance(data, dict):
                messages = (data.get("data") or {}).get("messages") or data.get("messages") or []
            else:
                messages = data or []
            if not messages:
                if attempt % 3 == 0:
                    logf(f"[*] 等待激活邮件 (yydsmail)... ({attempt * 2}s)")
                continue
            for msg in messages:
                subject = (msg.get("subject") or "")
                if "confirm" in subject.lower() or "novita" in subject.lower() or "verify" in subject.lower():
                    msg_id = msg.get("id", "")
                    detail = _get(f"{base_url}/v1/messages/{urllib.parse.quote(str(msg_id))}")
                    d = detail.get("data", detail) if isinstance(detail, dict) else {}
                    html = ""
                    if isinstance(d, dict):
                        for k in ("html", "text", "content", "body"):
                            html += " " + _flatten_mail_field(d.get(k))
                    token = _extract_novita_token(html)
                    if token:
                        logf("[+] 激活 token 已获取 (yydsmail)")
                        return token
        except Exception as e:
            if attempt % 3 == 0:
                logf(f"[*] yydsmail 出错: {e}, 重试中...")
    logf("[-] 激活邮件超时 (30s)")
    return None


async def _poll_duckmail_activation(email: str, mail_meta: dict, logf) -> Optional[str]:
    """从 DuckMail 提取 Novita 激活 token

    DuckMail API（meta 由 Go 端 duckmail.go 创建邮箱时给出 token/base_url）：
      GET {base}/messages         Bearer=<token>  → {"hydra:member":[{id,subject}]}
      GET {base}/messages/{id}    Bearer=<token>  → {text/html}
    """
    token = mail_meta.get("token")
    base = (mail_meta.get("base_url") or "https://api.duckmail.sbs").strip().rstrip("/")
    if not token:
        logf("[-] 无 DuckMail token")
        return None

    _ssl_ctx = ssl.create_default_context()
    _ssl_ctx.check_hostname = False
    _ssl_ctx.verify_mode = ssl.CERT_NONE

    def _get(url):
        req = urllib.request.Request(url, headers={
            "Authorization": f"Bearer {token}",
            "Accept": "application/json",
            "User-Agent": "Mozilla/5.0 (compatible; RegPlatform/1.0)",
        })
        with urllib.request.urlopen(req, context=_ssl_ctx, timeout=10) as resp:
            return json.loads(resp.read())

    for attempt in range(15):
        await asyncio.sleep(2)
        try:
            data = _get(f"{base}/messages")
            if isinstance(data, dict):
                members = data.get("hydra:member") or data.get("member") or data.get("messages") or []
            else:
                members = data or []
            if not members:
                if attempt % 3 == 0:
                    logf(f"[*] 等待激活邮件 (duckmail)... ({attempt * 2}s)")
                continue
            for msg in members:
                subject = (msg.get("subject") or "")
                if "confirm" in subject.lower() or "novita" in subject.lower() or "verify" in subject.lower():
                    detail = _get(f"{base}/messages/{urllib.parse.quote(str(msg.get('id', '')))}")
                    d2 = detail.get("data", detail) if isinstance(detail, dict) else {}
                    html = ""
                    if isinstance(d2, dict):
                        for k in ("html", "text", "content", "body"):
                            html += " " + _flatten_mail_field(d2.get(k))
                    token2 = _extract_novita_token(html)
                    if token2:
                        logf("[+] 激活 token 已获取 (duckmail)")
                        return token2
        except Exception as e:
            if attempt % 3 == 0:
                logf(f"[*] duckmail 出错: {e}, 重试中...")
    logf("[-] 激活邮件超时 (30s)")
    return None


async def _poll_outlook_activation(email: str, mail_meta: dict, logf) -> Optional[str]:
    """从 Outlook 账号池邮箱提取 Novita 激活 token（IMAP XOAUTH2，复用 outlook_mail）"""
    if poll_outlook_async is None:
        logf("[-] outlook_mail 模块不可用")
        return None
    cid = mail_meta.get("client_id", "")
    rt = mail_meta.get("refresh_token", "")
    if not cid or not rt:
        logf("[-] outlook meta 缺 client_id/refresh_token")
        return None
    logf("[*] 轮询 Outlook 收件箱 (IMAP)...")
    try:
        kind, val = await poll_outlook_async(email, cid, rt, "novita_token", 180, ["novita"])
    except Exception as e:
        logf(f"[-] outlook 收件出错: {e}")
        return None
    if val:
        logf("[+] 激活 token 已获取 (outlook)")
        return val
    logf("[-] 激活邮件超时 (outlook)")
    return None


def _flatten_mail_field(v) -> str:
    """邮件正文字段可能是 str / list / dict（不同邮箱 API 格式），统一拍平成字符串。"""
    if v is None:
        return ""
    if isinstance(v, str):
        return v
    if isinstance(v, list):
        return " ".join(_flatten_mail_field(x) for x in v)
    if isinstance(v, dict):
        return " ".join(_flatten_mail_field(x) for x in v.values())
    return str(v)


def _extract_novita_token(html: str) -> Optional[str]:
    """从邮件 HTML 中提取 Novita 激活 token"""
    match = re.search(r'token=([A-Za-z0-9_-]+)', html)
    return match.group(1) if match else None


# ─── HTTP 端点 ────────────────────────────────────────────────────────────

@app.before_serving
async def _init_semaphore():
    global _novita_semaphore
    _novita_semaphore = asyncio.Semaphore(_MAX_CONCURRENT)


@app.route("/novita/process", methods=["POST"])
async def novita_register():
    data = await request.get_json()
    if not data or not data.get("email"):
        return jsonify({"ok": False, "error": "invalid request"}), 400

    email = data["email"]
    password = data.get("password", "")
    if not password:
        # 生成安全密码: 字母+数字+特殊字符，打乱顺序
        alpha = [secrets.choice(string.ascii_letters) for _ in range(8)]
        digit = [secrets.choice(string.digits) for _ in range(3)]
        special = [secrets.choice("!@#$%&")]
        pwd_list = alpha + digit + special
        random.shuffle(pwd_list)
        password = "".join(pwd_list)
    proxy = data.get("proxy", "")
    solver_api = data.get("solver_api", "http://localhost:5072")
    mail_provider = data.get("mail_provider", "")
    mail_meta = data.get("mail_meta", {})
    ahem_base_url = data.get("ahem_base_url", "")
    yydsmail_url = data.get("yydsmail_url", "")
    yydsmail_key = data.get("yydsmail_key", "")

    logger.info("收到 novita 注册请求: %s solver=%s", email, solver_api)

    async def generate():
        async with _novita_semaphore:
            log_q: queue.Queue = queue.Queue()
            cancel_event = Event()

            reg_task = asyncio.create_task(
                _do_novita_register(
                    email=email,
                    password=password,
                    proxy=proxy,
                    solver_api=solver_api,
                    mail_provider=mail_provider,
                    mail_meta=mail_meta,
                    ahem_base_url=ahem_base_url,
                    yydsmail_url=yydsmail_url,
                    yydsmail_key=yydsmail_key,
                    log_q=log_q,
                    cancel=cancel_event,
                )
            )

            last_keepalive = asyncio.get_event_loop().time()
            try:
                while not reg_task.done():
                    await asyncio.sleep(0.3)
                    now = asyncio.get_event_loop().time()
                    while not log_q.empty():
                        try:
                            msg = log_q.get_nowait()
                            yield f"LOG:{msg}\n"
                            last_keepalive = now
                        except queue.Empty:
                            break
                    if now - last_keepalive > 10:
                        yield "LOG: .\n"
                        last_keepalive = now

                # 输出剩余日志
                while not log_q.empty():
                    try:
                        msg = log_q.get_nowait()
                        yield f"LOG:{msg}\n"
                    except queue.Empty:
                        break

                result = reg_task.result()
                yield json.dumps(result, ensure_ascii=False) + "\n"

            except asyncio.CancelledError:
                cancel_event.set()
                reg_task.cancel()
                yield json.dumps({"ok": False, "error": "cancelled"}) + "\n"

    return generate(), 200, {"Content-Type": "text/plain; charset=utf-8"}


@app.route("/health", methods=["GET"])
async def health():
    return jsonify({"status": "ok", "service": "novita"})


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=5002)
    args = parser.parse_args()
    app.run(host=args.host, port=args.port)
