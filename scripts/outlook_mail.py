#!/usr/bin/env python3
"""Outlook 收件统一实现（IMAP XOAUTH2）。

- 可作为模块被 novita_reg / fireworks_reg / openrouter_reg `from outlook_mail import poll_outlook` 直接调用。
- 也作为独立服务跑在 :5003，暴露 POST /outlook/poll，给 Go 侧（grok 等用 MultiProvider.FetchVerificationCode 的平台）调用。

收件原理：refresh_token + client_id → access_token（login.live.com / microsoftonline 双端点）
→ IMAP4_SSL outlook.office365.com:993 XOAUTH2 读 INBOX + Junk → 按 mode 提取。

mode:
  code         → 提取 6 位（兜底 4-8 位）验证码
  novita_token → 提取激活链接里的 token=xxx
  link         → 提取首个 https 链接（可用 senders 关键字过滤发件人/正文）
"""
import asyncio
import email as email_mod
import imaplib
import json
import re
import ssl
import time
import urllib.parse
import urllib.request

_SSL = ssl.create_default_context()
_SSL.check_hostname = False
_SSL.verify_mode = ssl.CERT_NONE

_TOKEN_ENDPOINTS = (
    "https://login.live.com/oauth20_token.srf",
    "https://login.microsoftonline.com/consumers/oauth2/v2.0/token",
)


def outlook_access_token(client_id: str, refresh_token: str) -> str:
    last = ""
    for url in _TOKEN_ENDPOINTS:
        data = urllib.parse.urlencode({
            "client_id": client_id,
            "refresh_token": refresh_token,
            "grant_type": "refresh_token",
        }).encode()
        req = urllib.request.Request(
            url, data=data, method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded"})
        try:
            with urllib.request.urlopen(req, timeout=20, context=_SSL) as r:
                j = json.loads(r.read())
                if j.get("access_token"):
                    return j["access_token"]
                last = str(j)[:200]
        except Exception as e:
            last = str(e)
    raise RuntimeError(f"outlook token refresh 失败: {last}")


def _msg_text(msg) -> str:
    out = []
    if msg.is_multipart():
        for part in msg.walk():
            if part.get_content_type() in ("text/plain", "text/html"):
                try:
                    out.append(part.get_payload(decode=True).decode(
                        part.get_content_charset() or "utf-8", "ignore"))
                except Exception:
                    pass
    else:
        try:
            out.append(msg.get_payload(decode=True).decode(
                msg.get_content_charset() or "utf-8", "ignore"))
        except Exception:
            pass
    return " ".join(out)


def _extract(body: str, mode: str):
    if mode == "novita_token":
        m = re.search(r'token=([A-Za-z0-9_\-]+)', body)
        return ("novita_token", m.group(1)) if m else (None, None)
    if mode == "link":
        m = re.search(r'https?://[^\s"\'<>]+', body)
        return ("link", m.group(0)) if m else (None, None)
    # 默认 code：优先 6 位，兜底 4-8 位
    m = re.search(r'(?<!\d)(\d{6})(?!\d)', body)
    if not m:
        m = re.search(r'(?<!\d)(\d{4,8})(?!\d)', body)
    return ("code", m.group(1)) if m else (None, None)


def _poll_loop(email_addr: str, client_id: str, refresh_token: str, on_body,
               timeout: int = 180, senders=None, cancel=None):
    """轮询 outlook INBOX/Junk，对每封新邮件正文调用 on_body(body)；返回首个 on_body 非空结果，否则 None。

    senders: 可选关键字列表，命中发件人或正文才算目标邮件（None 不过滤）。
    cancel: 可选，带 is_set() 的对象，置位则提前返回。
    """
    deadline = time.time() + timeout
    access = None
    seen = set()
    senders = [s.lower() for s in (senders or [])]
    while time.time() < deadline:
        if cancel is not None and getattr(cancel, "is_set", lambda: False)():
            return None
        try:
            if access is None:
                access = outlook_access_token(client_id, refresh_token)
            imap = imaplib.IMAP4_SSL("outlook.office365.com", 993, ssl_context=_SSL)
            auth = f"user={email_addr}\x01auth=Bearer {access}\x01\x01".encode()
            imap.authenticate("XOAUTH2", lambda _c: auth)
            for folder in ("INBOX", "Junk"):
                try:
                    imap.select(folder)
                    typ, data = imap.search(None, "ALL")
                    if not data or not data[0]:
                        continue
                    for mid in data[0].split()[-15:]:
                        if mid in seen:
                            continue
                        seen.add(mid)
                        typ, md = imap.fetch(mid, "(RFC822)")
                        if not md or not md[0]:
                            continue
                        msg = email_mod.message_from_bytes(md[0][1])
                        frm = str(msg.get("from", ""))
                        subj = str(msg.get("subject", ""))
                        body = subj + " " + _msg_text(msg)
                        if senders:
                            low = (frm + " " + body).lower()
                            if not any(k in low for k in senders):
                                continue
                        res = on_body(body)
                        if res:
                            try:
                                imap.logout()
                            except Exception:
                                pass
                            return res
                except Exception:
                    pass
            try:
                imap.logout()
            except Exception:
                pass
        except Exception:
            access = None  # token 可能过期，下轮重刷
        time.sleep(5)
    return None


def poll_outlook(email_addr: str, client_id: str, refresh_token: str,
                 mode: str = "code", timeout: int = 180, senders=None):
    """按 mode 提取，返回 (kind, value) 或 (None, None)。"""
    holder = {}

    def on_body(body):
        kind, val = _extract(body, mode)
        if val:
            holder["kind"] = kind
            return val
        return None

    val = _poll_loop(email_addr, client_id, refresh_token, on_body, timeout, senders)
    if val:
        return (holder.get("kind", mode), val)
    return (None, None)


def poll_outlook_extract(email_addr: str, client_id: str, refresh_token: str, extractor,
                         timeout: int = 180, senders=None, cancel=None):
    """用调用方的 extractor(body)->Optional[str] 提取（供 fireworks/openrouter 复用各自的提取器）。"""
    return _poll_loop(email_addr, client_id, refresh_token, extractor, timeout, senders, cancel)


async def poll_outlook_async(*args, **kwargs):
    """async 包装：把阻塞 IMAP 丢到线程，避免卡事件循环。"""
    return await asyncio.to_thread(poll_outlook, *args, **kwargs)


# ── 独立服务（:5003），给 Go 侧调用 ──────────────────────────────────────────
def _build_app():
    from quart import Quart, request, jsonify
    app = Quart(__name__)

    @app.route("/health")
    async def health():
        return jsonify({"status": "ok", "service": "outlook_mail"})

    @app.route("/outlook/poll", methods=["POST"])
    async def poll():
        body = await request.get_json(force=True, silent=True) or {}
        email_addr = body.get("email", "")
        cid = body.get("client_id", "")
        rt = body.get("refresh_token", "")
        mode = body.get("mode", "code")
        timeout = int(body.get("timeout", 180))
        senders = body.get("senders")
        if not (email_addr and cid and rt):
            return jsonify({"ok": False, "error": "email/client_id/refresh_token 必填"}), 400
        kind, val = await poll_outlook_async(email_addr, cid, rt, mode, timeout, senders)
        if val:
            return jsonify({"ok": True, "kind": kind, "value": val})
        return jsonify({"ok": False, "error": "未取到验证码/链接（超时）"})

    return app


if __name__ == "__main__":
    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="0.0.0.0")
    parser.add_argument("--port", type=int, default=5003)
    args = parser.parse_args()
    _build_app().run(host=args.host, port=args.port)
