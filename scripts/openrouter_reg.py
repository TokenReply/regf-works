"""
OpenRouter 全自动注册服务（Quart HTTP 端点）。

API:
  POST /openrouter/process  { email, password?, proxy?, solver_type?, solver_api?,
                               yescaptcha_key?, mail_provider?, mail_meta?,
                               yydsmail_url?, yydsmail_key?, ahem_base_url? }

流式响应：每行 LOG:xxx 为实时日志，最后一行为 JSON 注册结果。

Usage: python openrouter_reg.py [--host 0.0.0.0] [--port 5001]
"""

import argparse
import asyncio
import json
import logging
import os
import queue
import random
import re
import ssl
import string
import time
import urllib.error
import urllib.parse
import urllib.request
from threading import Event
from typing import Optional

import httpx

# ─── SSL ────────────────────────────────────────────────────────────────────
_SSL_CTX = ssl.create_default_context()
_SSL_CTX.check_hostname = False
_SSL_CTX.verify_mode = ssl.CERT_NONE

# ─── 日志 ────────────────────────────────────────────────────────────────────
logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("openrouter-reg")

# ─── 代理校验 ─────────────────────────────────────────────────────────────────
_PROXY_RE = re.compile(
    r'^(https?|socks[45])://[a-zA-Z0-9._\-:@]+(:\d{1,5})?(/[^\s]*)?$'
)


def _validate_proxy(proxy: str) -> Optional[str]:
    if not proxy:
        return None
    proxy = proxy.strip()
    if not _PROXY_RE.match(proxy):
        logger.warning("代理格式无效，已拒绝: %r", proxy[:100])
        return None
    if os.path.exists("/.dockerenv"):
        proxy = proxy.replace("127.0.0.1", "host.docker.internal")
        proxy = proxy.replace("localhost", "host.docker.internal")
    return proxy


# ─── 常量 ─────────────────────────────────────────────────────────────────────
CLERK_BASE = "https://clerk.openrouter.ai/v1"
CLERK_PARAMS = {"__clerk_api_version": "2025-11-10", "_clerk_js_version": "5.125.7"}
TURNSTILE_SITEKEY = "0x4AAAAAAAWXJGBD7bONzLBd"
TURNSTILE_PAGE_URL = "https://openrouter.ai/sign-up"

CLERK_HEADERS = {
    "Accept": "*/*",
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Content-Type": "application/x-www-form-urlencoded",
    "Origin": "https://openrouter.ai",
    "Referer": "https://openrouter.ai/",
    "Sec-Fetch-Dest": "empty",
    "Sec-Fetch-Mode": "cors",
    "Sec-Fetch-Site": "same-site",
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/146.0.0.0 Safari/537.36"
    ),
    "sec-ch-ua": '"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"',
    "sec-ch-ua-mobile": "?0",
    "sec-ch-ua-platform": '"Windows"',
}

_OR_HEADERS = {
    "Accept-Language": "zh-CN,zh;q=0.9",
    "Origin": "https://openrouter.ai",
    "sec-ch-ua": '"Chromium";v="146", "Not-A.Brand";v="24", "Google Chrome";v="146"',
    "sec-ch-ua-mobile": "?0",
    "sec-ch-ua-platform": '"Windows"',
    "User-Agent": (
        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
        "AppleWebKit/537.36 (KHTML, like Gecko) "
        "Chrome/146.0.0.0 Safari/537.36"
    ),
}

DEFAULT_YYDSMAIL_BASE_URL = "https://maliapi.215.im"

# ─── 邮件轮询超时（秒） ─────────────────────────────────────────────────────────
_MAIL_TIMEOUT = int(os.getenv("OPENROUTER_MAIL_TIMEOUT", "180"))

# ─── 并发控制 ─────────────────────────────────────────────────────────────────
_MAX_CONCURRENT = int(os.getenv("OPENROUTER_MAX_CONCURRENT", "30"))


# ─── 密码生成 ─────────────────────────────────────────────────────────────────

def gen_password(length: int = 14) -> str:
    chars = string.ascii_letters + string.digits + "!@#$%^&*"
    while True:
        pwd = "".join(random.choices(chars, k=length))
        if (any(c.isupper() for c in pwd)
                and any(c.islower() for c in pwd)
                and any(c.isdigit() for c in pwd)
                and any(c in "!@#$%^&*" for c in pwd)):
            return pwd


# ─── 验证链接提取 ─────────────────────────────────────────────────────────────

def _extract_verify_link(body: str) -> Optional[str]:
    if not body:
        return None
    body = body.replace("&amp;", "&").replace("&lt;", "<").replace("&gt;", ">")
    patterns = [
        r'href=["\']?(https://clerk\.openrouter\.ai/v1/verify\?[^"\'>\s]+)',
        r'(https://clerk\.openrouter\.ai/v1/verify\?[^\s"\'<>]+)',
    ]
    for pat in patterns:
        m = re.search(pat, body)
        if m:
            return m.group(1)
    return None


# ─── CAPTCHA Solvers ──────────────────────────────────────────────────────────

async def solve_turnstile(
    client: httpx.AsyncClient,
    solver_type: str = "selfhost",
    solver_api: str = "http://localhost:5072",
    yescaptcha_key: str = "",
    log_fn=None,
    proxy: str = "",
) -> str:
    if solver_type == "yescaptcha":
        return await _solve_yescaptcha(client, yescaptcha_key, log_fn)
    return await _solve_selfhost(client, solver_api, log_fn, proxy=proxy)


async def _solve_yescaptcha(
    client: httpx.AsyncClient,
    yescaptcha_key: str,
    log_fn=None,
) -> str:
    def lq(msg):
        if log_fn:
            log_fn(msg)

    yc = "https://api.yescaptcha.com"
    resp = await client.post(f"{yc}/createTask", json={
        "clientKey": yescaptcha_key,
        "task": {
            "type": "TurnstileTaskProxyless",
            "websiteURL": TURNSTILE_PAGE_URL,
            "websiteKey": TURNSTILE_SITEKEY,
        },
    }, timeout=30)
    resp.raise_for_status()
    data = resp.json()
    if data.get("errorId"):
        raise RuntimeError(f"YesCaptcha createTask: {data}")

    task_id = data["taskId"]
    lq(f"[captcha] yescaptcha task_id={task_id}")

    for _ in range(40):
        await asyncio.sleep(3)
        r = await client.post(f"{yc}/getTaskResult", json={
            "clientKey": yescaptcha_key,
            "taskId": task_id,
        }, timeout=30)
        r.raise_for_status()
        result = r.json()
        if result.get("status") == "ready":
            token = result["solution"]["token"]
            lq(f"[captcha] 成功，token 长度={len(token)}")
            return token
        if result.get("errorId"):
            raise RuntimeError(f"YesCaptcha error: {result}")

    raise TimeoutError("YesCaptcha 超时")


async def _solve_selfhost(
    client: httpx.AsyncClient,
    solver_api: str = "http://localhost:5072",
    log_fn=None,
    max_retries: int = 3,
    proxy: str = "",
) -> str:
    def lq(msg):
        if log_fn:
            log_fn(msg)

    base = solver_api.rstrip("/")

    for attempt in range(1, max_retries + 1):
        try:
            lq(f"[captcha] selfhost 第 {attempt}/{max_retries} 次尝试...")
            # Solver 在本地，不能走代理，用独立的无代理 client
            params = {"url": TURNSTILE_PAGE_URL, "sitekey": TURNSTILE_SITEKEY}
            if proxy:
                params["proxy"] = proxy
            async with httpx.AsyncClient(timeout=30) as solver_client:
                resp = await solver_client.get(f"{base}/turnstile", params=params)
            resp.raise_for_status()
            data = resp.json()
            if data.get("errorId"):
                lq(f"[captcha] solver 返回错误: {data}")
                continue

            task_id = data.get("taskId") or data.get("task_id")
            if not task_id:
                lq(f"[captcha] solver 无 task_id: {data}")
                continue
            lq(f"[captcha] selfhost task_id={task_id}")

            for poll in range(60):
                await asyncio.sleep(3)
                try:
                    async with httpx.AsyncClient(timeout=15) as solver_client:
                        r = await solver_client.get(f"{base}/result", params={"id": task_id})
                except Exception as e:
                    lq(f"[captcha] 轮询异常: {e}")
                    continue

                raw = r.text.strip()
                if not raw or raw == "CAPTCHA_NOT_READY":
                    continue

                try:
                    result = r.json()
                except Exception:
                    continue

                if not isinstance(result, dict):
                    continue

                if result.get("errorId"):
                    lq(f"[captcha] solver 错误: {result}")
                    break

                token = None
                solution = result.get("solution")
                if isinstance(solution, dict):
                    token = solution.get("token") or solution.get("value")
                if not token:
                    token = result.get("value") or result.get("token")

                if token:
                    fail_markers = ("CAPTCHA_FAIL", "FAIL", "ERROR", "TIMEOUT")
                    if token.upper() in fail_markers:
                        lq(f"[captcha] solver 打码失败: {token}")
                        break
                    lq(f"[captcha] 成功，token 长度={len(token)}")
                    return token

                if result.get("status") == "ready":
                    lq(f"[captcha] ready 但无 token: {result}")
                    break
            else:
                lq(f"[captcha] 第 {attempt} 次尝试超时")

        except httpx.HTTPStatusError as e:
            lq(f"[captcha] HTTP {e.response.status_code}: {e.response.text[:200]}")
        except Exception as e:
            lq(f"[captcha] 异常: {e}")

        if attempt < max_retries:
            lq("[captcha] 等待 5s 后重试...")
            await asyncio.sleep(5)

    raise RuntimeError(f"selfhost solver 全部 {max_retries} 次尝试失败")


# ─── Clerk API ────────────────────────────────────────────────────────────────

async def clerk_get(path: str, session: httpx.AsyncClient) -> dict:
    r = await session.get(
        f"{CLERK_BASE}{path}", params=CLERK_PARAMS, headers=CLERK_HEADERS, timeout=20
    )
    r.raise_for_status()
    return r.json()


async def clerk_post(path: str, data: dict, session: httpx.AsyncClient) -> dict:
    r = await session.post(
        f"{CLERK_BASE}{path}",
        params=CLERK_PARAMS,
        headers=CLERK_HEADERS,
        data=data,
        timeout=20,
    )
    body = r.json() if r.headers.get("content-type", "").startswith("application/json") else {"raw": r.text}
    if r.status_code >= 400:
        raise RuntimeError(f"POST {path} {r.status_code}: {json.dumps(body, ensure_ascii=False)}")
    return body


def _extract_signup_id(resp: dict) -> str:
    if "response" in resp and isinstance(resp["response"], dict):
        sid = resp["response"].get("id")
        if sid:
            return sid
    if "client" in resp:
        sid = resp["client"].get("sign_up", {}).get("id")
        if sid:
            return sid
    raise KeyError(f"无法从响应中找到 sign_up id: {list(resp.keys())}")


# ─── OpenRouter API Key 创建 ──────────────────────────────────────────────────

async def create_api_key(
    session_id: str,
    clerk_client: httpx.AsyncClient,
    proxy: Optional[str] = None,
    log_fn=None,
) -> str:
    def lq(msg):
        if log_fn:
            log_fn(msg)

    # 1. 获取短期 JWT
    token_resp = await clerk_post(
        f"/client/sessions/{session_id}/tokens",
        {"organization_id": ""},
        clerk_client,
    )
    jwt = token_resp["jwt"]
    lq(f"JWT 长度: {len(jwt)}")

    proxy_url = _validate_proxy(proxy) if proxy else None
    async with httpx.AsyncClient(
        follow_redirects=True, verify=False, timeout=30,
        proxy=proxy_url,
    ) as or_client:
        # 2. 设置 Clerk session cookie
        or_client.cookies.set("__session", jwt, domain=".openrouter.ai", path="/")
        or_client.cookies.set(
            "__client_uat",
            str(int(asyncio.get_event_loop().time())),
            domain=".openrouter.ai", path="/",
        )

        lq(f"Cookie __session: {jwt[:20]}...")

        # 2.5 访问主页以初始化 session
        try:
            home_resp = await or_client.get(
                "https://openrouter.ai/",
                headers={**_OR_HEADERS, "Accept": "text/html"},
                timeout=20,
            )
            lq(f"主页状态: {home_resp.status_code}")
        except Exception as e:
            lq(f"主页访问失败: {e}")

        # 3. 获取 workspaceId
        lq("正在获取 workspaceId...")
        html_resp = await or_client.get(
            "https://openrouter.ai/workspaces/default/keys",
            headers={**_OR_HEADERS, "Accept": "text/html"},
            timeout=20,
        )
        lq(f"HTML 响应状态: {html_resp.status_code}")

        if html_resp.status_code != 200:
            lq(f"HTML 响应内容: {html_resp.text[:500]}")
            raise RuntimeError(f"访问 keys 页面失败: {html_resp.status_code}")

        html_text = html_resp.text
        workspace_id = _parse_workspace_id(html_text, log_fn)
        lq(f"workspace_id: {workspace_id}")

        # 4. 从 HTML 提取 JS chunk URLs
        js_pattern = r'<script[^>]*src="(/_next/static/chunks/[^"]+\.js[^"]*)"'
        js_refs = re.findall(js_pattern, html_text)
        js_urls = [f"https://openrouter.ai{ref}" for ref in js_refs]
        js_urls = list(dict.fromkeys(js_urls))
        lq(f"发现 {len(js_urls)} 个 JS chunks")

        if len(js_urls) == 0:
            raise RuntimeError("未找到 JS chunks，无法继续")

        # 5. 扫描 JS chunks 找 key 创建的 Server Action ID
        action_id = await _discover_key_action_id(or_client, js_urls, log_fn)
        lq(f"action_id: {action_id}")

        # 6. RSC state tree
        state_tree = (
            "%5B%22%22%2C%7B%22children%22%3A%5B%22(user)%22%2C%7B%22children%22%3A%5B"
            "%22(workspace)%22%2C%7B%22children%22%3A%5B%22workspaces%22%2C%7B%22children"
            "%22%3A%5B%5B%22workspaceId%22%2C%22default%22%2C%22d%22%5D%2C%7B%22children"
            "%22%3A%5B%22keys%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2Cnull"
            "%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D"
            "%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D%7D%2Cnull%2Cnull%2Ctrue%5D"
        )

        # 7. POST 创建 Key
        lq("正在调用 createAPIKeySA...")
        body = json.dumps([{
            "name": "Default",
            "limit": None,
            "usageLimitType": None,
            "includeBYOKInLimit": False,
            "expiresAt": None,
            "workspaceId": workspace_id,
            "creatorUserId": "$undefined",
        }])

        key_resp = await or_client.post(
            "https://openrouter.ai/workspaces/default/keys",
            content=body,
            headers={
                **_OR_HEADERS,
                "accept": "text/x-component",
                "content-type": "text/plain;charset=UTF-8",
                "next-action": action_id,
                "next-router-state-tree": state_tree,
                "referer": "https://openrouter.ai/workspaces/default/keys",
                "sec-fetch-dest": "empty",
                "sec-fetch-mode": "cors",
                "sec-fetch-site": "same-origin",
            },
            timeout=20,
        )

        lq(f"创建 Key 响应状态: {key_resp.status_code}")

        if key_resp.status_code != 200:
            raise RuntimeError(f"创建 Key 失败: {key_resp.status_code} - {key_resp.text[:500]}")

        return _parse_api_key(key_resp.text)


def _parse_workspace_id(text: str, log_fn=None) -> str:
    def lq(msg):
        if log_fn:
            log_fn(msg)

    all_uuids = re.findall(
        r'([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})',
        text, re.IGNORECASE,
    )

    if not all_uuids:
        lq("[debug] 未找到任何 UUID")
        raise RuntimeError("无法解析 workspaceId")

    lq(f"[debug] 找到 {len(all_uuids)} 个 UUID")

    for uuid in all_uuids:
        pattern = rf'.{{0,100}}{re.escape(uuid)}.{{0,100}}'
        matches = re.findall(pattern, text, re.IGNORECASE | re.DOTALL)
        for match in matches:
            if 'workspace' in match.lower() or 'default' in match.lower():
                lq(f"[debug] 使用与 workspace 相关的 UUID: {uuid[:8]}...")
                return uuid

    lq(f"[debug] 使用第一个 UUID: {all_uuids[0][:8]}...")
    return all_uuids[0]


def _parse_api_key(text: str) -> str:
    m = re.search(r'"key"\s*:\s*"(sk-or-v1-[a-f0-9]+)"', text)
    if m:
        return m.group(1)
    raise RuntimeError(f"无法解析 API Key: {text[:300]}")


async def _discover_key_action_id(
    client: httpx.AsyncClient,
    js_urls: list[str],
    log_fn=None,
) -> str:
    def lq(msg):
        if log_fn:
            log_fn(msg)

    all_refs: list[tuple[str, str, str]] = []

    for url in js_urls:
        try:
            r = await client.get(url, timeout=10)
            text = r.text
            pattern = r'"([a-f0-9]{38,52})",[a-zA-Z_$.]+\.callServer[^"]*?"([^"]+)"'
            for m in re.finditer(pattern, text):
                hex_id, func_name = m.group(1), m.group(2)
                chunk_name = url.split("/")[-1]
                all_refs.append((hex_id, func_name, chunk_name))
        except Exception:
            continue

    if all_refs:
        lq(f"找到 {len(all_refs)} 个 Server Action")
        for hex_id, func_name, chunk in all_refs:
            lq(f"  {func_name} -> {hex_id[:16]}... ({chunk})")

    for hex_id, func_name, _ in all_refs:
        if "key" in func_name.lower() and "create" in func_name.lower():
            return hex_id

    for hex_id, func_name, _ in all_refs:
        if "key" in func_name.lower():
            return hex_id

    known_skip = {
        "saveAcquisitionSourceSA", "completeOnboardingSA",
        "invalidateCacheAction", "invalidateCache",
    }
    for hex_id, func_name, _ in all_refs:
        if "create" in func_name.lower() and func_name not in known_skip:
            return hex_id

    raise RuntimeError(
        f"无法找到 key 创建的 Server Action ID。"
        f"找到的 actions: {[(n, i[:16]) for i, n, _ in all_refs]}"
    )


# ─── 辅助：文本提取 ──────────────────────────────────────────────────────────

def _normalize_yydsmail_base_url(raw: str) -> str:
    base_url = (raw or "").strip().rstrip("/")
    if base_url.endswith("/v1"):
        base_url = base_url[:-3].rstrip("/")
    return base_url or DEFAULT_YYDSMAIL_BASE_URL


def _iter_text_chunks(value) -> list[str]:
    if isinstance(value, str):
        return [value] if value else []
    if isinstance(value, list):
        return [item for item in value if isinstance(item, str) and item]
    return []


# ─── 邮件轮询：AHEM ──────────────────────────────────────────────────────────

def _poll_ahem(
    base_url: str,
    email: str,
    meta: dict,
    timeout: int = 180,
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
) -> Optional[str]:
    """轮询 AHEM 邮箱，从邮件中提取 Clerk 验证链接。"""
    prefix = meta.get("prefix", "")
    if not prefix:
        parts = email.split("@", 1)
        prefix = parts[0] if parts else ""
    if not base_url:
        base_url = meta.get("base_url", "")
    if not base_url:
        logger.warning("[%s] ahem: 缺少 base_url", email)
        return None

    base_url = base_url.rstrip("/")
    if not base_url.endswith("/api"):
        base_url += "/api"

    list_url = f"{base_url}/mailbox/{prefix}/email"
    start = time.time()
    attempt = 0

    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            elapsed = int(time.time() - start)
            log_q.put(f"等待验证邮件 (ahem)... ({elapsed}s / {timeout}s)")
        try:
            req = urllib.request.Request(list_url)
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                emails_list = json.loads(resp.read())

            if isinstance(emails_list, list) and len(emails_list) > 0:
                for mail_item in emails_list:
                    email_id = mail_item.get("emailId", "")
                    subject = mail_item.get("subject", "") or ""

                    link = _extract_verify_link(subject)
                    if link:
                        logger.info("[%s] ahem subject 提取验证链接", email)
                        return link

                    if email_id:
                        try:
                            detail_url = f"{base_url}/mailbox/{prefix}/email/{email_id}"
                            dreq = urllib.request.Request(detail_url)
                            dreq.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
                            with urllib.request.urlopen(dreq, timeout=10, context=_SSL_CTX) as dresp:
                                detail = json.loads(dresp.read())
                            for field in ("text", "html", "textAsHtml"):
                                content = detail.get(field)
                                if content and isinstance(content, str):
                                    link = _extract_verify_link(content)
                                    if link:
                                        logger.info("[%s] ahem %s 提取验证链接", email, field)
                                        return link
                        except Exception as e:
                            logger.debug("[%s] ahem 详情获取失败(id=%s): %s", email, email_id, e)

        except Exception as exc:
            logger.debug("[%s] ahem 轮询出错(第%d次): %s", email, attempt, exc)

        if cancel is not None:
            if cancel.wait(3):
                return None
        else:
            time.sleep(3)
    return None


# ─── 邮件轮询：YYDS Mail ─────────────────────────────────────────────────────

def _poll_yydsmail(
    yydsmail_url: str,
    yydsmail_key: str,
    email: str,
    meta: dict,
    timeout: int = 180,
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
) -> Optional[str]:
    """轮询 YYDS Mail API，从邮件中提取 Clerk 验证链接。"""
    token = meta.get("token", "")
    if not token:
        logger.warning("[%s] yydsmail: 缺少 token", email)
        return None

    base_url = _normalize_yydsmail_base_url(yydsmail_url)
    list_url = f"{base_url}/v1/messages?address={urllib.parse.quote(email)}"
    start = time.time()
    attempt = 0
    consecutive_errors = 0
    last_checked_at: dict[str, float] = {}
    recheck_interval = 5.0

    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            elapsed = int(time.time() - start)
            log_q.put(f"等待验证邮件 (yydsmail)... ({elapsed}s / {timeout}s)")
        try:
            req = urllib.request.Request(list_url)
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            req.add_header("Authorization", f"Bearer {token}")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                data = json.loads(resp.read())

            if not data.get("success"):
                consecutive_errors += 1
                if consecutive_errors >= 3:
                    logger.warning("[%s] yydsmail 连续错误，放弃", email)
                    return None
                time.sleep(2)
                continue

            consecutive_errors = 0
            messages = data.get("data", {}).get("messages", [])
            for msg in messages:
                msg_id = msg.get("id", "")
                if msg_id:
                    last_checked = last_checked_at.get(msg_id, 0.0)
                    if last_checked and time.time() - last_checked < recheck_interval:
                        continue
                    last_checked_at[msg_id] = time.time()

                subject = msg.get("subject", "") or ""
                link = _extract_verify_link(subject)
                if link:
                    logger.info("[%s] yydsmail subject 提取验证链接 (第 %d 次)", email, attempt)
                    return link

                if msg_id:
                    try:
                        detail_url = f"{base_url}/v1/messages/{urllib.parse.quote(msg_id, safe='')}"
                        dreq = urllib.request.Request(detail_url)
                        dreq.add_header("Authorization", f"Bearer {token}")
                        dreq.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
                        with urllib.request.urlopen(dreq, timeout=10, context=_SSL_CTX) as dresp:
                            detail = json.loads(dresp.read())
                        if detail.get("success"):
                            d = detail.get("data", {})
                            for field in ("text", "html"):
                                for text in _iter_text_chunks(d.get(field, "")):
                                    link = _extract_verify_link(text)
                                    if link:
                                        logger.info(
                                            "[%s] yydsmail %s 提取验证链接 (第 %d 次)",
                                            email, field, attempt,
                                        )
                                        return link
                    except Exception as e:
                        logger.debug("[%s] yydsmail 详情获取失败(id=%s): %s", email, msg_id, e)

        except urllib.error.HTTPError as exc:
            logger.warning("[%s] yydsmail HTTP %s (第%d次)", email, exc.code, attempt)
            consecutive_errors += 1
            if consecutive_errors >= 3:
                return None
        except Exception as exc:
            logger.warning("[%s] yydsmail 轮询出错(第%d次): %s", email, attempt, exc)
            consecutive_errors += 1
            if consecutive_errors >= 5:
                return None

        if cancel is not None:
            if cancel.wait(2):
                return None
        else:
            time.sleep(2)
    return None


# ─── 主注册流程 ──────────────────────────────────────────────────────────────

async def _do_openrouter_register(
    email: str,
    password: str,
    proxy: Optional[str],
    solver_type: str,
    solver_api: str,
    yescaptcha_key: str,
    mail_provider: str,
    mail_meta: Optional[dict],
    yydsmail_url: str,
    yydsmail_key: str,
    ahem_base_url: str,
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
) -> dict:
    """OpenRouter 注册全流程。"""

    def lq(msg: str):
        logger.info("[%s] openrouter: %s", email, msg)
        if log_q is not None:
            log_q.put(msg)

    def _cancelled() -> bool:
        return cancel is not None and cancel.is_set()

    password = password or gen_password()

    proxy_url = _validate_proxy(proxy) if proxy else None

    try:
        async with httpx.AsyncClient(
            follow_redirects=True, verify=False, proxy=proxy_url,
        ) as client:

            # 1. 初始化 Clerk
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq("[1] 初始化 Clerk...")
            await clerk_get("/environment", client)
            await clerk_get("/client", client)

            # 2. 解 CAPTCHA
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq(f"[2] 解 Turnstile CAPTCHA (solver={solver_type})...")
            captcha_token = await solve_turnstile(
                client,
                solver_type=solver_type,
                solver_api=solver_api,
                yescaptcha_key=yescaptcha_key,
                log_fn=lq,
                proxy=proxy_url or "",
            )

            # 3. 提交注册
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq(f"[3] 提交注册 email={email}...")
            signup_resp = await clerk_post("/client/sign_ups", {
                "email_address": email,
                "password": password,
                "legal_accepted": "true",
                "locale": "zh-CN",
                "captcha_token": captcha_token,
                "captcha_widget_type": "smart",
            }, client)

            errors = signup_resp.get("errors", [])
            for err in errors:
                if err.get("code") == "form_email_address_blocked":
                    domain = email.split("@")[-1]
                    return {
                        "ok": False,
                        "error": f"域名 {domain} 被 Clerk 拦截为临时邮箱",
                        "retriable": False,
                    }

            signup_id = _extract_signup_id(signup_resp)
            lq(f"sign_up id: {signup_id}")

            # 4. 触发验证邮件 (email_link)
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq("[4] 触发验证邮件 (email_link)...")
            redirect = (
                "https://openrouter.ai/sign-up#/verify"
                "?sign_up_force_redirect_url=https%3A%2F%2Fopenrouter.ai%2F%3F"
                "&sign_in_force_redirect_url=https%3A%2F%2Fopenrouter.ai%2F%3F"
            )
            await clerk_post(f"/client/sign_ups/{signup_id}/prepare_verification", {
                "strategy": "email_link",
                "redirect_url": redirect,
            }, client)

            # 5. 轮询邮箱获取验证链接
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq(f"[5] 等待验证邮件 (provider={mail_provider or 'yydsmail'})...")

            if mail_provider == "ahem":
                verify_link = await asyncio.to_thread(
                    _poll_ahem,
                    ahem_base_url, email, mail_meta or {},
                    timeout=max(_MAIL_TIMEOUT, 180),
                    log_q=log_q, cancel=cancel,
                )
            else:
                verify_link = await asyncio.to_thread(
                    _poll_yydsmail,
                    yydsmail_url, yydsmail_key, email, mail_meta or {},
                    timeout=max(_MAIL_TIMEOUT, 180),
                    log_q=log_q, cancel=cancel,
                )

            if not verify_link:
                return {
                    "ok": False,
                    "error": f"验证邮件超时 (provider={mail_provider or 'yydsmail'})",
                    "retriable": True,
                }
            lq(f"验证链接: {verify_link[:80]}...")

            # 6. 访问验证链接
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq("[6] 验证邮箱...")
            await client.get(verify_link, timeout=20)

            # 7. 获取 session
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq("[7] 获取 session...")
            client_resp = await clerk_get("/client", client)
            sessions = (
                client_resp.get("response", {}).get("sessions")
                or client_resp.get("client", {}).get("sessions")
                or []
            )
            if not sessions:
                return {
                    "ok": False,
                    "error": "验证后未找到 session，可能验证未成功",
                    "retriable": True,
                }
            session_id = sessions[0]["id"]
            lq(f"session id: {session_id}")

            # 8. touch session
            lq("[8] Touch session...")
            await clerk_post(f"/client/sessions/{session_id}/touch", {
                "active_organization_id": "",
                "intent": "select_session",
            }, client)

            # 9. 创建 API Key
            if _cancelled():
                return {"ok": False, "error": "任务已取消"}
            lq("[9] 创建 API Key...")
            api_key = await create_api_key(session_id, client, proxy, log_fn=lq)
            lq(f"API Key: {api_key[:20]}...")

        return {
            "ok": True,
            "email": email,
            "password": password,
            "api_key": api_key,
            "session_id": session_id,
        }

    except asyncio.CancelledError:
        return {"ok": False, "error": "任务已取消"}
    except Exception as exc:
        logger.error("[%s] openrouter 注册异常: %s", email, exc, exc_info=True)
        return {"ok": False, "error": str(exc), "retriable": True}


# ─── Quart 应用 ──────────────────────────────────────────────────────────────

from quart import Quart, jsonify, request

app = Quart(__name__)
_openrouter_semaphore: asyncio.Semaphore = None  # type: ignore


@app.before_serving
async def _init_semaphore():
    global _openrouter_semaphore
    _openrouter_semaphore = asyncio.Semaphore(_MAX_CONCURRENT)


@app.route("/openrouter/process", methods=["POST"])
async def openrouter_register():
    data = await request.get_json()
    if not data or not data.get("email"):
        return jsonify({"ok": False, "error": "invalid request"}), 400

    email = data["email"]
    password = data.get("password", "")
    proxy = data.get("proxy")
    solver_type = data.get("solver_type", "selfhost")
    solver_api = data.get("solver_api", "http://localhost:5072")
    yescaptcha_key = data.get("yescaptcha_key", "")
    mail_provider = data.get("mail_provider", "")
    mail_meta = data.get("mail_meta", {})
    yydsmail_url = data.get("yydsmail_url", "")
    yydsmail_key = data.get("yydsmail_key", "")
    ahem_base_url = data.get("ahem_base_url", "")

    logger.info("收到 openrouter 注册请求: %s proxy=%s solver=%s",
                email, "***" if proxy else "none", solver_type)

    async def generate():
        async with _openrouter_semaphore:
            log_q: queue.Queue = queue.Queue()
            cancel_event = Event()

            reg_task = asyncio.create_task(
                _do_openrouter_register(
                    email=email,
                    password=password,
                    proxy=proxy,
                    solver_type=solver_type,
                    solver_api=solver_api,
                    yescaptcha_key=yescaptcha_key,
                    mail_provider=mail_provider,
                    mail_meta=mail_meta,
                    yydsmail_url=yydsmail_url,
                    yydsmail_key=yydsmail_key,
                    ahem_base_url=ahem_base_url,
                    log_q=log_q,
                    cancel=cancel_event,
                )
            )

            last_keepalive = asyncio.get_event_loop().time()
            try:
                while not reg_task.done():
                    drained = False
                    while True:
                        try:
                            msg = log_q.get_nowait()
                            yield f"LOG:{msg}\n"
                            drained = True
                            last_keepalive = asyncio.get_event_loop().time()
                        except queue.Empty:
                            break
                    now = asyncio.get_event_loop().time()
                    if not drained and now - last_keepalive > 10:
                        yield "LOG: .\n"
                        last_keepalive = now
                    if not drained:
                        await asyncio.sleep(0.2)

                # 排空剩余日志
                while True:
                    try:
                        msg = log_q.get_nowait()
                        yield f"LOG:{msg}\n"
                    except queue.Empty:
                        break

                result = await reg_task
                yield json.dumps(result, ensure_ascii=False) + "\n"

            except (asyncio.CancelledError, GeneratorExit):
                cancel_event.set()
                reg_task.cancel()
                try:
                    await reg_task
                except (asyncio.CancelledError, Exception):
                    pass
                logger.info("[%s] openrouter 客户端断开，注册任务已取消", email)
                raise

    return generate(), 200, {"Content-Type": "text/plain; charset=utf-8"}


@app.route("/health", methods=["GET"])
async def health():
    return jsonify({"status": "ok", "service": "openrouter-reg"})


# ─── 入口 ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="OpenRouter registration service")
    parser.add_argument("--host", default=os.getenv("OPENROUTER_REG_HOST", "0.0.0.0"))
    parser.add_argument("--port", type=int, default=int(os.getenv("OPENROUTER_REG_PORT", "5001")))
    args = parser.parse_args()

    app.run(host=args.host, port=args.port)
