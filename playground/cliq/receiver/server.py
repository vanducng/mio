#!/usr/bin/env python3
"""Cliq Participation Handler receiver — stdlib only.

POST /cliq      verify HMAC, append event to JSONL, echo to stdout
GET  /healthz   200 ok
"""
from __future__ import annotations

import base64
import hashlib
import hmac
import json
import logging
import os
import sys
import threading
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from cliq_client import CliqClient

LISTEN_HOST = os.environ.get("LISTEN_HOST", "0.0.0.0")
LISTEN_PORT = int(os.environ.get("LISTEN_PORT", "8765"))
WEBHOOK_SECRET = os.environ.get("WEBHOOK_SECRET", "").encode()
LOG_PATH = Path(os.environ.get("LOG_PATH", "/appdata/messages.jsonl"))
SIG_HEADER = "X-Webhook-Signature"  # value: "sha256=<hex>"
# Bot id is the org-unique anchor in mention payloads. Display name can
# collide; unique_name isn't carried in mention objects. Set ZOHO_CLIQ_BOT_ID
# from the bot's edit URL: https://cliq.zoho.com/company/<org>/integrations/bots/<bot_id>
BOT_ID = os.environ.get("ZOHO_CLIQ_BOT_ID", "").strip()
BOT_NAME = os.environ.get("ZOHO_CLIQ_BOT_NAME", "tobytime").lower()
cliq = CliqClient()

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s %(levelname)s %(message)s",
    stream=sys.stdout,
)
log = logging.getLogger("cliq-receiver")


def verify_signature(body: bytes, header: str | None) -> bool:
    """Accept either hex or base64 encoded HMAC-SHA256.

    Deluge's zoho.encryption.hmacsha256 returns base64; openssl/curl test
    helpers usually emit hex. Compare against both to support both senders.
    """
    if not WEBHOOK_SECRET:
        log.warning("WEBHOOK_SECRET unset — accepting all requests (dev only)")
        return True
    if not header:
        return False
    digest = hmac.new(WEBHOOK_SECRET, body, hashlib.sha256).digest()
    expected_hex = digest.hex()
    expected_b64 = base64.b64encode(digest).decode()
    received = header.split("=", 1)[1] if header.startswith("sha256=") else header
    if hmac.compare_digest(received, expected_hex) or hmac.compare_digest(received, expected_b64):
        return True
    log.warning(
        "sig mismatch. expected_hex=%s expected_b64=%s received=%s body_len=%d body[:80]=%r",
        expected_hex, expected_b64, header, len(body), body[:80],
    )
    return False


def append_jsonl(record: dict) -> None:
    LOG_PATH.parent.mkdir(parents=True, exist_ok=True)
    with LOG_PATH.open("a", encoding="utf-8") as fh:
        fh.write(json.dumps(record, ensure_ascii=False) + "\n")


class Handler(BaseHTTPRequestHandler):
    def log_message(self, fmt: str, *args) -> None:  # quiet default access log
        log.info("%s - %s", self.client_address[0], fmt % args)

    def _send(self, status: int, body: dict | str = "") -> None:
        payload = json.dumps(body) if isinstance(body, dict) else body
        encoded = payload.encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(encoded)))
        self.end_headers()
        self.wfile.write(encoded)

    def do_GET(self) -> None:
        if self.path == "/healthz":
            self._send(200, {"ok": True})
            return
        self._send(404, {"error": "not found"})

    def do_POST(self) -> None:
        if self.path != "/cliq":
            self._send(404, {"error": "not found"})
            return
        length = int(self.headers.get("Content-Length", "0"))
        body = self.rfile.read(length) if length else b""
        if not verify_signature(body, self.headers.get(SIG_HEADER)):
            log.warning("signature mismatch from %s", self.client_address[0])
            self._send(401, {"error": "bad signature"})
            return
        try:
            event = json.loads(body) if body else {}
        except json.JSONDecodeError:
            self._send(400, {"error": "invalid json"})
            return

        record = {
            "received_at": datetime.now(timezone.utc).isoformat(),
            "event": event,
        }
        append_jsonl(record)

        op = event.get("operation", "?")
        chat = (event.get("chat") or {}).get("title") or (event.get("chat") or {}).get("id")
        user = (event.get("user") or {}).get("name") or (event.get("user") or {}).get("id")
        msg = (event.get("data") or {}).get("message") or {}
        text = msg.get("text", "") or msg.get("comment", "")
        log.info("op=%s chat=%s user=%s text=%r", op, chat, user, text[:200])

        # Reply asynchronously when the bot is @mentioned, so we ack Cliq fast.
        if op == "message_sent" and is_bot_mentioned(msg):
            chat_obj = event.get("chat") or {}
            channel = chat_obj.get("channel_unique_name")
            chat_id = chat_obj.get("id")
            msg_id = msg.get("id")
            if channel and chat_id and msg_id:
                threading.Thread(
                    target=handle_bot_mention,
                    args=(channel, chat_id, msg_id, user, text),
                    daemon=True,
                ).start()

        self._send(200, {"ok": True})


def is_bot_mentioned(message: dict) -> bool:
    """True when our bot appears in the message's mentions array.

    Prefers exact id match (BOT_ID, set in env) since bot display names can
    collide across an org. Falls back to display-name match only if BOT_ID
    is unset, with a warning so it gets fixed.
    """
    for m in message.get("mentions") or []:
        if m.get("type") != "bot":
            continue
        if BOT_ID and m.get("id") == BOT_ID:
            return True
        if not BOT_ID:
            name = (m.get("name") or "").lower()
            dname = (m.get("dname") or "").lstrip("@").lower()
            if BOT_NAME in (name, dname):
                log.warning("matched bot mention by name (set ZOHO_CLIQ_BOT_ID for safety)")
                return True
    return False


def handle_bot_mention(
    channel: str,
    chat_id: str,
    msg_id: str,
    user: str | None,
    text: str,
) -> None:
    """React 👀 first (acknowledge), then post a quoted reply as the bot."""
    log.info("bot mention in #%s by %s (msg=%s) — reacting + replying", channel, user, msg_id)
    cliq.react_to_message(chat_id, msg_id, "👀")
    reply = f"echo: {text}" if text else "echo: (no text)"
    cliq.post_to_channel(channel, reply, reply_to=msg_id)


def main() -> None:
    server = ThreadingHTTPServer((LISTEN_HOST, LISTEN_PORT), Handler)
    log.info("cliq-receiver listening on %s:%s, log=%s", LISTEN_HOST, LISTEN_PORT, LOG_PATH)
    if not WEBHOOK_SECRET:
        log.warning("running WITHOUT signature verification — set WEBHOOK_SECRET")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        log.info("shutting down")
        server.shutdown()


if __name__ == "__main__":
    main()
