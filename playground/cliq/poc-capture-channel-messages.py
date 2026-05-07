#!/usr/bin/env python3
"""
PoC: poll the Zoho Cliq REST API to capture messages from #DucDev channel.

Strategy:
  1. Refresh OAuth access token (1h TTL) using stored refresh token.
  2. Poll GET /api/v2/chats/{chat_id}/messages on a fixed interval.
  3. Track last seen message_id; print only new messages each tick.

Why polling for the PoC:
  - Bot Participation Handler is the production answer (push), but it needs
    Deluge code + bot config in the Cliq dashboard. Polling validates the
    credential / scope plumbing end-to-end with zero Cliq-side changes.

Required OAuth scopes:
  ZohoCliq.OrganizationMessages.READ   (the key one for chats/{id}/messages)
  ZohoCliq.Chats.READ                  (list chats / cursor follow)

Run:
  ./poc-capture-channel-messages.py            # poll #DucDev every 5s
  ./poc-capture-channel-messages.py --once     # one-shot fetch + exit
  ./poc-capture-channel-messages.py --interval 10
"""

from __future__ import annotations

import argparse
import json
import os
import signal
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

# #DucDev channel — IDs from the Cliq Connectors panel
DUCDEV_CHAT_ID = "CT_1608777491574236080_637446511"
DUCDEV_CHANNEL_NAME = "ducdev"

CLIQ_API = "https://cliq.zoho.com/api/v2"
ZOHO_OAUTH = "https://accounts.zoho.com/oauth/v2/token"

SCRIPT_DIR = Path(__file__).resolve().parent
SECRETS_FILE = SCRIPT_DIR / "secrets.env"


def load_secrets(path: Path) -> dict[str, str]:
    """Parse simple KEY=VALUE .env file."""
    secrets: dict[str, str] = {}
    if not path.exists():
        sys.exit(f"secrets file not found: {path}")
    for raw in path.read_text().splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        secrets[key.strip()] = value.strip().strip('"').strip("'")
    return secrets


def http_request(
    url: str,
    *,
    method: str = "GET",
    headers: dict[str, str] | None = None,
    body: bytes | None = None,
    timeout: int = 15,
) -> tuple[int, dict[str, Any] | str]:
    """Minimal HTTP client. Returns (status, parsed_json_or_text)."""
    req = urllib.request.Request(url, data=body, method=method, headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read().decode("utf-8")
            status = resp.status
    except urllib.error.HTTPError as e:
        raw = e.read().decode("utf-8", errors="replace")
        status = e.code
    try:
        return status, json.loads(raw)
    except json.JSONDecodeError:
        return status, raw


def refresh_access_token(secrets: dict[str, str]) -> str:
    """Exchange refresh token for a fresh access token (1h TTL)."""
    payload = urllib.parse.urlencode({
        "grant_type": "refresh_token",
        "client_id": secrets["ZOHO_CLIQ_CLIENT_ID"],
        "client_secret": secrets["ZOHO_CLIQ_CLIENT_SECRET"],
        "refresh_token": secrets["ZOHO_CLIQ_REFRESH_TOKEN"],
    }).encode()
    status, data = http_request(
        ZOHO_OAUTH,
        method="POST",
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        body=payload,
    )
    if status != 200 or not isinstance(data, dict) or "access_token" not in data:
        sys.exit(f"token refresh failed (HTTP {status}): {data}")
    granted = data.get("scope", "")
    if "OrganizationMessages.READ" not in granted and "Messages.READ" not in granted:
        print(
            "WARNING: token lacks ZohoCliq.OrganizationMessages.READ — "
            "re-run setup-zoho-cliq-oauth.sh after updating SCOPES.",
            file=sys.stderr,
        )
        print(f"granted scopes: {granted}", file=sys.stderr)
    return data["access_token"]


def fetch_messages(access_token: str, chat_id: str, limit: int = 20) -> list[dict[str, Any]]:
    """GET /api/v2/chats/{chat_id}/messages — newest-first per Cliq docs."""
    url = f"{CLIQ_API}/chats/{chat_id}/messages?limit={limit}"
    status, data = http_request(
        url,
        headers={"Authorization": f"Zoho-oauthtoken {access_token}"},
    )
    if status != 200:
        sys.exit(f"messages fetch failed (HTTP {status}): {data}")
    if isinstance(data, dict):
        # Cliq wraps the list; key has varied — handle both shapes seen in v2.
        for key in ("data", "messages"):
            if isinstance(data.get(key), list):
                return data[key]
        # Fall back: if it looks like a single envelope with list-typed values.
        for value in data.values():
            if isinstance(value, list):
                return value
    return data if isinstance(data, list) else []


def format_message(msg: dict[str, Any]) -> str:
    """Compact one-line summary for stdout."""
    sender = msg.get("sender", {})
    sender_name = sender.get("name") or sender.get("id") or "?"
    ts = msg.get("time") or msg.get("timestamp") or ""
    msg_type = msg.get("type", "text")
    content = msg.get("content", {})
    if isinstance(content, dict):
        text = content.get("text") or json.dumps(content, ensure_ascii=False)
    else:
        text = str(content)
    return f"[{ts}] <{sender_name}> ({msg_type}) {text}"


def poll_loop(
    secrets: dict[str, str],
    chat_id: str,
    interval: float,
    once: bool,
    raw: bool,
) -> None:
    access_token = refresh_access_token(secrets)
    token_acquired_at = time.time()
    seen_ids: set[str] = set()
    first_pass = True

    def renew_if_stale() -> str:
        # Cliq access tokens last 1h; refresh proactively at 50min.
        nonlocal access_token, token_acquired_at
        if time.time() - token_acquired_at > 50 * 60:
            access_token = refresh_access_token(secrets)
            token_acquired_at = time.time()
        return access_token

    print(f"polling chat_id={chat_id} every {interval}s (Ctrl-C to stop)")
    while True:
        token = renew_if_stale()
        try:
            messages = fetch_messages(token, chat_id, limit=20)
        except SystemExit:
            raise
        except Exception as exc:  # network blip, retry next tick
            print(f"fetch error: {exc!r}", file=sys.stderr)
            messages = []

        # API returns newest-first; iterate oldest→newest for natural log order.
        new_messages = []
        for msg in reversed(messages):
            mid = msg.get("id") or msg.get("message_id")
            if not mid or mid in seen_ids:
                continue
            seen_ids.add(mid)
            new_messages.append(msg)

        if first_pass:
            # On startup, mark history as seen but still show the most recent few.
            print(f"--- {len(new_messages)} message(s) in current window ---")
            for msg in new_messages:
                print(json.dumps(msg, ensure_ascii=False) if raw else format_message(msg))
            first_pass = False
        else:
            for msg in new_messages:
                print(json.dumps(msg, ensure_ascii=False) if raw else format_message(msg))

        if once:
            return
        time.sleep(interval)


def main() -> None:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--chat-id", default=DUCDEV_CHAT_ID, help="Cliq chat_id (default: #DucDev)")
    parser.add_argument("--interval", type=float, default=5.0, help="Poll interval in seconds")
    parser.add_argument("--once", action="store_true", help="Single fetch and exit")
    parser.add_argument("--raw", action="store_true", help="Print raw JSON per message")
    args = parser.parse_args()

    secrets = load_secrets(SECRETS_FILE)
    for required in ("ZOHO_CLIQ_CLIENT_ID", "ZOHO_CLIQ_CLIENT_SECRET", "ZOHO_CLIQ_REFRESH_TOKEN"):
        if not secrets.get(required):
            sys.exit(f"missing {required} in {SECRETS_FILE}")

    signal.signal(signal.SIGINT, lambda *_: sys.exit("\nstopped"))

    poll_loop(
        secrets=secrets,
        chat_id=args.chat_id,
        interval=args.interval,
        once=args.once,
        raw=args.raw,
    )


if __name__ == "__main__":
    main()
