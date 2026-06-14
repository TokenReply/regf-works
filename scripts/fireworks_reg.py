"""
Fireworks.ai 纯 HTTP 注册服务（独立 Quart 端点）。

API:
  POST /fireworks/process  { email, password?, proxy?, yydsmail_url, yydsmail_key,
                             mail_provider?, mail_meta? }

流式响应：每行 LOG:xxx 为实时日志，最后一行为 JSON 注册结果。

Usage: python fireworks_reg.py [--host 0.0.0.0] [--port 5000]
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
import ssl
import string
import time
import urllib.error
import urllib.parse
import urllib.request
from threading import Event
from typing import Optional

# ─── 代理链支持 ─────────────────────────────────────────────────────────────
try:
    from proxy_chain import chain_proxy
except ImportError:
    def chain_proxy(p):
        return p or ""

# ─── curl_cffi（lazy import in _fw_make_session） ────────────────────────────

# ─── SSL ────────────────────────────────────────────────────────────────────
_SSL_CTX = ssl.create_default_context()
_SSL_CTX.check_hostname = False
_SSL_CTX.verify_mode = ssl.CERT_NONE

# ─── 日志 ────────────────────────────────────────────────────────────────────
logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")
logger = logging.getLogger("fireworks-reg")

# ─── 代理校验 ─────────────────────────────────────────────────────────────────
_PROXY_RE = re.compile(
    r'^(https?|socks[45])://[a-zA-Z0-9._\-:@]+(:\d{1,5})?(/[^\s]*)?$'
)


def _validate_proxy(proxy: str) -> Optional[str]:
    """校验代理 URL 格式，防止恶意字符串注入。"""
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
FIRST_NAMES = [
    "James", "Mary", "Robert", "Patricia", "John", "Jennifer", "Michael", "Linda",
    "David", "Elizabeth", "William", "Barbara", "Richard", "Susan", "Joseph", "Jessica",
    "Thomas", "Sarah", "Christopher", "Karen", "Charles", "Lisa", "Daniel", "Nancy",
    "Emma", "Oliver", "Sophia", "Liam", "Ava", "Noah", "Isabella", "Ethan",
    "Mia", "Mason", "Charlotte", "Logan", "Amelia", "Lucas", "Harper", "Aiden",
]
LAST_NAMES = [
    "Smith", "Johnson", "Williams", "Brown", "Jones", "Garcia", "Miller", "Davis",
    "Rodriguez", "Martinez", "Hernandez", "Lopez", "Gonzalez", "Wilson", "Anderson",
    "Taylor", "Thomas", "Jackson", "White", "Harris", "Martin", "Thompson", "Moore",
]

DEFAULT_YYDSMAIL_BASE_URL = "https://maliapi.215.im"

# ─── Fireworks 常量 ───────────────────────────────────────────────────────────
FIREWORKS_APP_BASE = "https://app.fireworks.ai"
FIREWORKS_COGNITO_BASE = "https://cognito-idp.us-west-2.amazonaws.com/"

# Server Action IDs — 跟 fireworks 前端部署哈希强耦合，升级时需要重新抓
# 最后更新: 2026-06-10
FIREWORKS_ACTION_SIGNUP      = "408aa536b5191228ace5bd1baade455dd72b26da9e"  # submitSignupForm
FIREWORKS_ACTION_LOGIN       = "40b4d5b71a361f6278169582309e9ddfa97cc039cd"  # submitLoginForm
FIREWORKS_ACTION_ONBOARDING  = "60a001e530b99884536040f375194661abde8dfd92"  # submitOnboardingForm
FIREWORKS_ACTION_CREATE_KEY  = "783c458fd58f7e0316b2ffb972b89d44a08adc792e"  # createUserAction (2026-06-12 更新)

FIREWORKS_ROUTER_SIGNUP     = '%5B%22%22%2C%7B%22children%22%3A%5B%22(v2-auth)%22%2C%7B%22children%22%3A%5B%22signup%22%2C%7B%22children%22%3A%5B%22__PAGE__%22%2C%7B%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%5D%7D%2Cnull%2Cnull%2Ctrue%5D'
FIREWORKS_ROUTER_LOGIN      = '[""%2C{"children":["(v2-auth)"%2C{"children":["login"%2C{"children":["email"%2C{"children":["__PAGE__"%2C{}]}]}]}]}%2Cnull%2Cnull%2Ctrue]'
FIREWORKS_ROUTER_ONBOARDING = '[""%2C{"children":["(v2-auth)"%2C{"children":["onboarding"%2C{"children":["__PAGE__"%2C{}]}]}]}%2Cnull%2Cnull%2Ctrue]'
FIREWORKS_ROUTER_APIKEYS    = '[""%2C{"children":["(console)"%2C{"children":["settings"%2C{"children":["users"%2C{"children":["api-keys"%2C{"children":["__PAGE__"%2C{}]}]}]}]}]}%2Cnull%2Cnull%2Ctrue]'

_FW_CONFIRM_LINK_RE = re.compile(
    r"https?://[^\s<>\"]*?/signup/confirm\?[^\s<>\"]+",
    re.IGNORECASE,
)

# ─── 邮件轮询超时（秒） ─────────────────────────────────────────────────────────
_MAIL_TIMEOUT = int(os.getenv("FIREWORKS_MAIL_TIMEOUT", "180"))

# ─── 并发控制 ─────────────────────────────────────────────────────────────────
_MAX_CONCURRENT = int(os.getenv("FIREWORKS_MAX_CONCURRENT", "30"))


# ─── 辅助：文本提取 ──────────────────────────────────────────────────────────
def _normalize_sender(sender_obj) -> str:
    """把不同 provider 返回的发件人结构归一成邮箱字符串。"""
    if isinstance(sender_obj, dict):
        return str(sender_obj.get("address", "") or sender_obj.get("name", "") or "")
    if isinstance(sender_obj, list):
        for item in sender_obj:
            sender = _normalize_sender(item)
            if sender:
                return sender
        return ""
    return str(sender_obj) if sender_obj else ""


def _iter_text_chunks(value) -> list[str]:
    """统一展开 text/html 字段，兼容字符串和字符串数组。"""
    if isinstance(value, str):
        return [value] if value else []
    if isinstance(value, list):
        return [item for item in value if isinstance(item, str) and item]
    return []


def _normalize_yydsmail_base_url(raw: str) -> str:
    base_url = (raw or "").strip().rstrip("/")
    if base_url.endswith("/v1"):
        base_url = base_url[:-3].rstrip("/")
    return base_url or DEFAULT_YYDSMAIL_BASE_URL


# ─── YYDSMail 轮询（简化版） ─────────────────────────────────────────────────
def _poll_yydsmail(yydsmail_url: str, yydsmail_key: str, email: str, meta: dict,
                   timeout: int = 180,
                   log_q: Optional[queue.Queue] = None,
                   cancel: Optional[Event] = None,
                   extractor=None,
                   progress_label: str = "确认链接") -> Optional[str]:
    """轮询 YYDS Mail API 获取邮件并用 extractor 提取内容。"""
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
    _extractor = extractor or (lambda t: t if t else None)

    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            elapsed = int(time.time() - start)
            log_q.put(f"等待{progress_label} (yydsmail)... ({elapsed}s / {timeout}s)")
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
                # 先从 subject 提取
                code = _extractor(subject)
                if code:
                    logger.info("[%s] yydsmail subject 提取结果: %s (第 %d 次)", email, code, attempt)
                    return code

                # 获取邮件详情
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
                                    code = _extractor(text)
                                    if code:
                                        logger.info("[%s] yydsmail %s 提取结果: %s (第 %d 次)",
                                                    email, field, code, attempt)
                                        return code
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


def _poll_ahem(base_url: str, email: str, meta: dict,
               timeout: int = 180,
               log_q: Optional[queue.Queue] = None,
               cancel: Optional[Event] = None,
               extractor=None,
               progress_label: str = "确认链接") -> Optional[str]:
    """轮询 AHEM 邮箱服务获取邮件并用 extractor 提取内容。"""
    prefix = meta.get("prefix", "")
    if not prefix:
        parts = email.split("@", 1)
        prefix = parts[0] if parts else ""
    if not base_url:
        base_url = meta.get("base_url", "")
    if not base_url:
        logger.warning("[%s] ahem: 缺少 base_url", email)
        return None
    # 确保以 /api 结尾
    base_url = base_url.rstrip("/")
    if not base_url.endswith("/api"):
        base_url += "/api"

    list_url = f"{base_url}/mailbox/{prefix}/email"
    start = time.time()
    attempt = 0
    _extractor = extractor or (lambda t: t if t else None)

    while time.time() - start < timeout:
        if cancel is not None and cancel.is_set():
            return None
        attempt += 1
        if log_q is not None and attempt % 5 == 1 and attempt > 1:
            elapsed = int(time.time() - start)
            log_q.put(f"等待{progress_label} (ahem)... ({elapsed}s / {timeout}s)")
        try:
            req = urllib.request.Request(list_url)
            req.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
            with urllib.request.urlopen(req, timeout=10, context=_SSL_CTX) as resp:
                emails_list = json.loads(resp.read())

            if isinstance(emails_list, list) and len(emails_list) > 0:
                for mail_item in emails_list:
                    email_id = mail_item.get("emailId", "")
                    subject = mail_item.get("subject", "") or ""
                    # 先从 subject 提取
                    code = _extractor(subject)
                    if code:
                        logger.info("[%s] ahem subject 提取结果: %s", email, code)
                        return code

                    # 获取邮件详情
                    if email_id:
                        try:
                            detail_url = f"{base_url}/mailbox/{prefix}/email/{email_id}"
                            dreq = urllib.request.Request(detail_url)
                            dreq.add_header("User-Agent", "Mozilla/5.0 (compatible; RegPlatform/1.0)")
                            with urllib.request.urlopen(dreq, timeout=10, context=_SSL_CTX) as dresp:
                                detail = json.loads(dresp.read())
                            # AHEM 详情格式: {text: "...", html: "...", subject: "..."}
                            for field in ("text", "html", "textAsHtml"):
                                content = detail.get(field)
                                if content and isinstance(content, str):
                                    code = _extractor(content)
                                    if code:
                                        logger.info("[%s] ahem %s 提取结果: %s", email, field, code)
                                        return code
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


# ─── Fireworks 辅助函数 ──────────────────────────────────────────────────────

def _fw_generate_password() -> str:
    """生成符合 fireworks 密码策略的密码：>=8 位，含大小写 / 数字 / 特殊字符。"""
    specials = "!@#$%^&*-_="
    body = "".join(secrets.choice(string.ascii_letters + string.digits) for _ in range(10))
    return "Fw" + body + secrets.choice(specials) + secrets.choice(string.digits)


def _fw_generate_account_id() -> str:
    """accountId 必须 3-20 字符、小写字母开头、非连续连字符。"""
    return "fw" + "".join(secrets.choice(string.ascii_lowercase + string.digits) for _ in range(10))


def _fw_default_ua() -> str:
    return (
        "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 "
        "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"
    )


def _fw_build_urllib_opener(proxy: Optional[str]):
    valid = _validate_proxy(proxy) if proxy else ""
    chained = chain_proxy(valid) if valid else ""
    if chained:
        return urllib.request.build_opener(
            urllib.request.ProxyHandler({"http": chained, "https": chained}),
            urllib.request.HTTPSHandler(context=_SSL_CTX),
        )
    return urllib.request.build_opener(
        urllib.request.HTTPSHandler(context=_SSL_CTX),
    )


def _fw_make_session(proxy: Optional[str]):
    """创建一个带 cookie 的 curl_cffi Session，自动走链路代理。"""
    from curl_cffi import requests as cureq

    sess = cureq.Session(impersonate="chrome124")
    sess.headers.update({
        "User-Agent": _fw_default_ua(),
        "Accept-Language": "en-US,en;q=0.9",
        "Origin": FIREWORKS_APP_BASE,
        "Referer": FIREWORKS_APP_BASE + "/signup",
    })

    valid = _validate_proxy(proxy) if proxy else ""
    chained = chain_proxy(valid) if valid else ""
    if chained:
        sess.proxies = {"http": chained, "https": chained}
    return sess


class FireworksAccountSuspended(RuntimeError):
    """当 api.fireworks.ai 返回 412 + 'suspended' 时抛出。"""
    def __init__(self, code: str, body: str):
        super().__init__(f"suspended (HTTP {code}): {body[:200]}")
        self.code = code
        self.body = body


def _fw_api_json_request(path: str, api_key: str, proxy: Optional[str], timeout: float = 20.0) -> dict:
    req = urllib.request.Request(f"https://api.fireworks.ai{path}")
    req.add_header("Authorization", f"Bearer {api_key}")
    req.add_header("Accept", "application/json")
    req.add_header("User-Agent", _fw_default_ua())

    opener = _fw_build_urllib_opener(proxy)
    try:
        with opener.open(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8", errors="ignore")
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="ignore")
        if exc.code == 412 and "suspended" in raw.lower():
            raise FireworksAccountSuspended(str(exc.code), raw)
        raise RuntimeError(f"{path} HTTP {exc.code}: {raw[:300]}")
    except Exception as exc:
        raise RuntimeError(f"{path} 请求失败: {exc}")

    try:
        data = json.loads(raw) if raw else {}
    except Exception as exc:
        raise RuntimeError(f"{path} JSON 解析失败: {exc} raw={raw[:200]}")
    if not isinstance(data, dict):
        raise RuntimeError(f"{path} 返回格式异常: {type(data).__name__}")
    return data


def _fw_extract_account_id(account: dict) -> str:
    for key in ("accountId", "account_id", "id"):
        value = account.get(key)
        if isinstance(value, str) and value.strip():
            return value.strip()

    name = account.get("name")
    if isinstance(name, str):
        raw = name.strip()
        if raw.startswith("accounts/"):
            return raw.split("/", 1)[1].strip()
        if raw:
            return raw
    return ""


def _fw_summarize_quotas(quotas: list[dict]) -> list[dict]:
    summary: list[dict] = []
    for quota in quotas[:12]:
        if not isinstance(quota, dict):
            continue
        item = {}
        for key in ("name", "value", "maxValue", "usage", "updateTime"):
            if key in quota and quota[key] not in (None, ""):
                item[key] = quota[key]
        if item:
            summary.append(item)
    return summary


def _fw_collect_account_state(api_key: str, email: str, proxy: Optional[str]) -> dict:
    """用官方管理 API 回查账号真实状态。"""
    accounts_data = _fw_api_json_request("/v1/accounts", api_key, proxy, timeout=20.0)
    accounts = accounts_data.get("accounts") or []
    if not isinstance(accounts, list) or not accounts:
        raise RuntimeError("List Accounts 返回空列表")

    lowered_email = (email or "").strip().lower()
    account = None
    for item in accounts:
        if not isinstance(item, dict):
            continue
        if (item.get("email") or "").strip().lower() == lowered_email:
            account = item
            break
    if account is None:
        for item in accounts:
            if isinstance(item, dict):
                account = item
                break
    if account is None:
        raise RuntimeError("List Accounts 未返回可解析账号")

    account_id = _fw_extract_account_id(account)
    result = {
        "account_id": account_id,
        "account_name": account.get("name") or "",
        "account_email": account.get("email") or "",
        "account_display_name": account.get("displayName") or "",
        "account_type": account.get("accountType") or "",
        "account_state": account.get("state") or "",
        "suspend_state": account.get("suspendState") or "",
        "account_status_code": ((account.get("status") or {}).get("code") or "") if isinstance(account.get("status"), dict) else "",
        "account_status_message": ((account.get("status") or {}).get("message") or "") if isinstance(account.get("status"), dict) else "",
    }

    if account_id:
        quota_path = f"/v1/accounts/{urllib.parse.quote(account_id, safe='')}/quotas"
        quotas_data = _fw_api_json_request(quota_path, api_key, proxy, timeout=20.0)
        quotas = quotas_data.get("quotas") or []
        if isinstance(quotas, list):
            result["quota_summary"] = _fw_summarize_quotas(quotas)
            result["quota_names"] = [
                q.get("name", "")
                for q in quotas
                if isinstance(q, dict) and isinstance(q.get("name"), str)
            ][:20]
    return result


def _fw_verify_apikey_live(api_key: str, proxy: Optional[str]) -> tuple[bool, str]:
    """最终验活：用 api_key 直接打 /inference/v1/models。"""
    try:
        data = _fw_api_json_request("/inference/v1/models", api_key, proxy, timeout=15.0)
    except FireworksAccountSuspended as exc:
        return False, f"suspended: {exc.body[:150]}"
    except RuntimeError as exc:
        return False, str(exc)[:200]

    if isinstance(data, dict) and isinstance(data.get("data"), list):
        return True, f"models={len(data['data'])}"
    return False, f"unexpected payload: {str(data)[:120]}"


async def _fw_post_action(sess, *, path: str, action_id: str, router_state: str,
                          payload_json: str, timeout: float = 30.0) -> dict:
    """发起一个 Next.js Server Action 调用，从 RSC 流里提取 `1:{...}` 那一行结果。"""
    headers = {
        "Content-Type": "text/plain;charset=UTF-8",
        "Accept": "text/x-component",
        "Next-Action": action_id,
        "Next-Router-State-Tree": router_state,
    }
    url = FIREWORKS_APP_BASE + path

    def _do():
        return sess.post(url, data=payload_json, headers=headers, timeout=timeout)

    resp = await asyncio.to_thread(_do)
    if resp.status_code != 200:
        raise RuntimeError(f"{path} HTTP {resp.status_code}: {resp.text[:200]}")

    text = resp.text
    parsed: dict[str, dict] = {}
    for line in text.splitlines():
        if not line or ":" not in line:
            continue
        ref, _, body = line.partition(":")
        ref = ref.strip()
        body = body.strip()
        if not body.startswith("{"):
            continue
        try:
            parsed[ref] = json.loads(body)
        except Exception:
            continue

    # 情况 1：'0:' 是 `{"a":"$@<N>","f":...,"b":...}` → N 是真正的结果行
    root = parsed.get("0")
    if root and isinstance(root, dict):
        a_ref = root.get("a", "")
        if isinstance(a_ref, str) and a_ref.startswith("$@"):
            target = a_ref[2:]
            if target in parsed:
                return parsed[target]

    # 情况 2：直接取 '1:' 行
    if "1" in parsed:
        return parsed["1"]

    # 情况 3：兜底——找第一个含业务字段的 JSON
    business_keys = {"success", "error", "key", "keyId", "redirectPath", "serializedError", "userSub"}
    for ref, obj in parsed.items():
        if ref == "0":
            continue
        if isinstance(obj, dict) and business_keys & obj.keys():
            return obj

    raise RuntimeError(f"{path} 响应里未找到 action 结果: {text[:400]}")


def _fw_parse_confirm_link(link: str) -> Optional[tuple[str, str, str]]:
    """从 `/signup/confirm?client_id=...&user_name=...&confirmation_code=...` 里拆三元组。"""
    try:
        u = urllib.parse.urlparse(link)
        qs = urllib.parse.parse_qs(u.query)
        client_id = (qs.get("client_id") or [""])[0]
        user_name = (qs.get("user_name") or [""])[0]
        code = (qs.get("confirmation_code") or [""])[0]
        if client_id and user_name and code:
            return client_id, user_name, code
    except Exception:
        return None
    return None


def _fw_extract_confirm_payload(text: str) -> Optional[tuple[str, str, str]]:
    """在邮件正文 / subject / html 里找 fireworks 的确认链接。"""
    if not text:
        return None
    for m in _FW_CONFIRM_LINK_RE.finditer(text):
        link = m.group(0).rstrip(").,;\"'&")
        parsed = _fw_parse_confirm_link(link)
        if parsed:
            return parsed
    return None


def _fw_cognito_confirm(client_id: str, username: str, code: str,
                        proxy: Optional[str]) -> tuple[bool, str]:
    """直接调 Cognito ConfirmSignUp，跳过 /signup/confirm 页面的 JS 执行。"""
    body = json.dumps({
        "ClientId": client_id,
        "Username": username,
        "ConfirmationCode": code,
    })
    req = urllib.request.Request(FIREWORKS_COGNITO_BASE, data=body.encode("utf-8"))
    req.add_header("Content-Type", "application/x-amz-json-1.1")
    req.add_header("X-Amz-Target", "AWSCognitoIdentityProviderService.ConfirmSignUp")
    req.add_header("User-Agent", _fw_default_ua())

    opener = _fw_build_urllib_opener(proxy)

    try:
        with opener.open(req, timeout=20) as resp:
            _ = resp.read()
            return True, ""
    except urllib.error.HTTPError as exc:
        raw = exc.read().decode("utf-8", errors="ignore")
        return False, f"HTTP {exc.code}: {raw[:200]}"
    except Exception as exc:
        return False, str(exc)


# ─── 主注册流程 ──────────────────────────────────────────────────────────────

async def _do_fireworks_register(
    email: str,
    password: str,
    proxy: Optional[str],
    yydsmail_url: str,
    yydsmail_key: str,
    log_q: Optional[queue.Queue] = None,
    cancel: Optional[Event] = None,
    mail_provider: str = "",
    mail_meta: Optional[dict] = None,
) -> dict:
    """fireworks.ai 邮箱注册（纯 HTTP / 无 Playwright）。"""
    def lq(msg: str):
        logger.info("[%s] fireworks: %s", email, msg)
        if log_q is not None:
            log_q.put(msg)

    def _cancelled() -> bool:
        return cancel is not None and cancel.is_set()

    password = password or _fw_generate_password()
    first_name = random.choice(FIRST_NAMES)
    last_name = random.choice(LAST_NAMES)
    account_id = _fw_generate_account_id()
    user_sub = ""

    sess = None
    try:
        sess = _fw_make_session(proxy)

        # 1. 提交 signup
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("提交 signup server action...")
        signup_body = json.dumps([{"email": email, "password": password}])
        try:
            signup_resp = await _fw_post_action(
                sess,
                path="/signup",
                action_id=FIREWORKS_ACTION_SIGNUP,
                router_state=FIREWORKS_ROUTER_SIGNUP,
                payload_json=signup_body,
                timeout=30,
            )
        except Exception as exc:
            lq(f"signup 调用失败: {exc}")
            return {"ok": False, "error": f"signup 调用失败: {exc}", "retriable": True}

        if not signup_resp.get("success"):
            err = signup_resp.get("error", {})
            code = (err.get("code") or "").lower() if isinstance(err, dict) else ""
            msg = err.get("message") if isinstance(err, dict) else str(err)
            retriable = code in ("usernameexists", "toomanyrequests", "servererror", "")
            lq(f"signup 失败: code={code} msg={msg}")
            return {"ok": False, "error": f"signup: {msg or code}", "retriable": retriable}

        user_sub = signup_resp.get("userSub", "")
        lq(f"signup 成功: userSub={user_sub[:8]}...")

        # 2. 等邮件 → 抓确认链接
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq(f"等待 fireworks 确认邮件... (provider={mail_provider or 'yydsmail'})")

        confirm_triple_holder: list[tuple[str, str, str]] = []

        def _fw_extractor(text: str) -> Optional[str]:
            parsed = _fw_extract_confirm_payload(text)
            if parsed:
                confirm_triple_holder.append(parsed)
                return parsed[2]
            return None

        # 根据 mail_provider 选择轮询函数
        if mail_provider == "ahem":
            ahem_base_url = (mail_meta or {}).get("base_url", "")
            code = await asyncio.to_thread(
                _poll_ahem,
                ahem_base_url, email, mail_meta or {},
                timeout=max(_MAIL_TIMEOUT, 180),
                log_q=log_q, cancel=cancel,
                extractor=_fw_extractor, progress_label="确认链接",
            )
        else:
            code = await asyncio.to_thread(
                _poll_yydsmail,
                yydsmail_url, yydsmail_key, email, mail_meta or {},
                timeout=max(_MAIL_TIMEOUT, 180),
                log_q=log_q, cancel=cancel,
                extractor=_fw_extractor, progress_label="确认链接",
            )
        if not code or not confirm_triple_holder:
            return {"ok": False, "error": f"确认邮件超时 (provider={mail_provider or 'yydsmail'})",
                    "retriable": True}
        client_id, confirm_user_name, confirm_code = confirm_triple_holder[-1]
        lq(f"confirm link 拿到: user_name={confirm_user_name[:8]}... code={confirm_code}")

        # 3. 直接调 Cognito 完成 ConfirmSignUp
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        ok, cerr = await asyncio.to_thread(
            _fw_cognito_confirm, client_id, confirm_user_name, confirm_code, proxy
        )
        if not ok:
            lq(f"ConfirmSignUp 失败: {cerr}")
            return {"ok": False, "error": f"ConfirmSignUp 失败: {cerr}", "retriable": True}
        lq("账号已确认 (Cognito)")

        # 4. 登录
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        lq("提交登录...")
        login_body = json.dumps([{"email": email.lower(), "password": password}])
        try:
            login_resp = await _fw_post_action(
                sess,
                path="/login/email",
                action_id=FIREWORKS_ACTION_LOGIN,
                router_state=FIREWORKS_ROUTER_LOGIN,
                payload_json=login_body,
                timeout=30,
            )
        except Exception as exc:
            return {"ok": False, "error": f"login 调用失败: {exc}", "retriable": True}
        if not login_resp.get("success"):
            err = login_resp.get("error", {}) or {}
            return {"ok": False, "error": f"login: {err.get('message') or err.get('code')}",
                    "retriable": True}
        lq(f"登录成功, redirectPath={login_resp.get('redirectPath')}")

        # 5. 完成 onboarding
        if _cancelled():
            return {"ok": False, "error": "任务已取消"}
        requested_account_id = account_id
        lq(f"提交 onboarding (account_id={account_id}, skip=false)...")
        ob_payload = [{
            "accountId": account_id,
            "companyName": "",
            "firstName": first_name,
            "lastName": last_name,
            "agreeToTerms": True,
            "step": "questionnaire",
            "goals": [1],
            "useCases": [1],
            "otherGoal": "",
            "otherUseCases": "",
        }, False]
        for attempt in range(3):
            try:
                ob_resp = await _fw_post_action(
                    sess,
                    path="/onboarding",
                    action_id=FIREWORKS_ACTION_ONBOARDING,
                    router_state=FIREWORKS_ROUTER_ONBOARDING,
                    payload_json=json.dumps(ob_payload),
                    timeout=30,
                )
            except Exception as exc:
                return {"ok": False, "error": f"onboarding 调用失败: {exc}", "retriable": True}

            serialized_err = ob_resp.get("serializedError")
            if serialized_err:
                if "accountId" in str(serialized_err).lower() or "already" in str(serialized_err).lower():
                    account_id = _fw_generate_account_id()
                    ob_payload[0]["accountId"] = account_id
                    lq(f"accountId 冲突, 换用 {account_id} 重试...")
                    continue
                return {"ok": False, "error": f"onboarding: {serialized_err}", "retriable": False}
            break
        else:
            return {"ok": False, "error": "onboarding 重试次数耗尽", "retriable": True}
        lq(f"onboarding 完成, redirectPath={ob_resp.get('redirectPath')}")

        # 6. 创建 API key
        lq("创建 API key...")
        # payload 格式: ["<数字>", "$undefined", false, "$undefined"]
        # 第一个参数是 key 编号，新账号首个 key 用 "1"
        create_key_payload = json.dumps(["1", "$undefined", False, "$undefined"])
        api_key = None
        for key_attempt in range(3):
            try:
                key_resp = await _fw_post_action(
                    sess,
                    path="/settings/users/api-keys",
                    action_id=FIREWORKS_ACTION_CREATE_KEY,
                    router_state=FIREWORKS_ROUTER_APIKEYS,
                    payload_json=create_key_payload,
                    timeout=30,
                )
            except Exception as exc:
                lq(f"创建 API key 失败 (第{key_attempt+1}次): {exc}")
                if key_attempt < 2:
                    await asyncio.sleep(2)
                    continue
                # 3 次都失败，返回注册成功但无 key
                return {
                    "ok": True,
                    "email": email,
                    "password": password,
                    "account_id": account_id,
                    "requested_account_id": requested_account_id,
                    "apikey": None,
                    "warning": f"注册成功但创建 API key 失败: {exc}",
                }

            # 解析 key_resp: {"keyId": "key_xxx", "key": "fw_xxx"}
            fw_key = key_resp.get("key", "")
            key_id = key_resp.get("keyId", "")
            if fw_key and fw_key.startswith("fw_"):
                api_key = fw_key
                lq(f"API key 创建成功: {api_key[:12]}... (keyId={key_id})")
                break
            elif key_resp.get("serializedError"):
                lq(f"创建 key 返回错误: {key_resp['serializedError']}")
                if key_attempt < 2:
                    await asyncio.sleep(2)
                    continue
            else:
                lq(f"创建 key 返回意外格式: {key_resp}")
                if key_attempt < 2:
                    await asyncio.sleep(2)
                    continue
                break

        if not api_key:
            return {
                "ok": True,
                "email": email,
                "password": password,
                "account_id": account_id,
                "requested_account_id": requested_account_id,
                "apikey": None,
                "warning": "注册成功但未能获取 API key，请手动登录创建",
            }

        # 7. 验活 API key
        lq("验活 API key...")
        try:
            live_ok, live_msg = _fw_verify_apikey_live(api_key, proxy)
        except Exception as exc:
            live_ok, live_msg = False, str(exc)[:150]
        lq(f"验活结果: ok={live_ok}, msg={live_msg}")

        # 8. 收集账号状态
        account_state = {}
        try:
            account_state = _fw_collect_account_state(api_key, email, proxy)
            lq(f"账号状态: suspend={account_state.get('suspend_state', 'N/A')}")
        except Exception as exc:
            lq(f"账号状态查询失败(非致命): {exc}")

        return {
            "ok": True,
            "email": email,
            "password": password,
            "account_id": account_id,
            "requested_account_id": requested_account_id,
            "apikey": api_key,
            "key_id": key_id,
            "live_ok": live_ok,
            "live_msg": live_msg,
            **account_state,
        }





    except asyncio.CancelledError:
        return {"ok": False, "error": "任务已取消"}
    except Exception as exc:
        logger.error("[%s] fireworks 注册异常: %s", email, exc, exc_info=True)
        return {"ok": False, "error": str(exc), "retriable": True}
    finally:
        if sess is not None:
            try:
                sess.close()
            except Exception:
                pass


# ─── Quart 应用 ──────────────────────────────────────────────────────────────

from quart import Quart, jsonify, request

app = Quart(__name__)
app.config["RESPONSE_TIMEOUT"] = 600  # 10 分钟
_fireworks_semaphore: asyncio.Semaphore = None  # type: ignore


@app.before_serving
async def _init_semaphore():
    global _fireworks_semaphore
    _fireworks_semaphore = asyncio.Semaphore(_MAX_CONCURRENT)


@app.route("/fireworks/process", methods=["POST"])
async def fireworks_register():
    data = await request.get_json()
    if not data or not data.get("email"):
        return jsonify({"ok": False, "error": "invalid request"}), 400

    email = data["email"]
    password = data.get("password", "")
    proxy = data.get("proxy")
    yydsmail_url = data.get("yydsmail_url", "")
    yydsmail_key = data.get("yydsmail_key", "")
    mail_provider = data.get("mail_provider", "")
    mail_meta = data.get("mail_meta", {})

    logger.info("收到 fireworks 注册请求: %s proxy=%s", email, "***" if proxy else "none")

    async def generate():
        async with _fireworks_semaphore:
            log_q: queue.Queue = queue.Queue()
            cancel_event = Event()

            reg_task = asyncio.create_task(
                _do_fireworks_register(
                    email, password, proxy, yydsmail_url, yydsmail_key,
                    log_q, cancel_event, mail_provider, mail_meta,
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
                logger.info("[%s] fireworks 客户端断开，注册任务已取消", email)
                raise

    return generate(), 200, {"Content-Type": "text/plain; charset=utf-8"}


@app.route("/health", methods=["GET"])
async def health():
    return jsonify({"status": "ok", "service": "fireworks-reg"})


# ─── 入口 ────────────────────────────────────────────────────────────────────

if __name__ == "__main__":
    parser = argparse.ArgumentParser(description="Fireworks registration service")
    parser.add_argument("--host", default=os.getenv("FIREWORKS_REG_HOST", "0.0.0.0"))
    parser.add_argument("--port", type=int, default=int(os.getenv("FIREWORKS_REG_PORT", "5000")))
    args = parser.parse_args()

    app.run(host=args.host, port=args.port)
