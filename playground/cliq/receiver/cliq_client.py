"""Tiny Zoho Cliq client — OAuth refresh + post-as-bot to channel.

Reads creds from env (loaded from secrets.env via compose env_file).
"""
from __future__ import annotations

import json
import logging
import os
import threading
import time
import urllib.parse
import urllib.request

log = logging.getLogger("cliq-client")

CLIQ_API = "https://cliq.zoho.com/api/v2"
ZOHO_OAUTH = "https://accounts.zoho.com/oauth/v2/token"


class CliqClient:
    """Thread-safe OAuth-token cache + channel post helper."""

    def __init__(self) -> None:
        self.client_id = os.environ.get("ZOHO_CLIQ_CLIENT_ID", "")
        self.client_secret = os.environ.get("ZOHO_CLIQ_CLIENT_SECRET", "")
        # Prefer the playground-scoped token (broader scopes incl. reactions)
        # so we never run on the production refresh token by accident.
        self.refresh_token = (
            os.environ.get("ZOHO_CLIQ_REFRESH_TOKEN_PLAYGROUND", "").strip()
            or os.environ.get("ZOHO_CLIQ_REFRESH_TOKEN", "").strip()
        )
        self.bot_name = os.environ.get("ZOHO_CLIQ_BOT_NAME", "tobytime")
        self._access_token: str | None = None
        self._expires_at: float = 0.0
        self._lock = threading.Lock()

    @property
    def configured(self) -> bool:
        return bool(self.client_id and self.client_secret and self.refresh_token)

    def _refresh(self) -> str:
        payload = urllib.parse.urlencode({
            "grant_type": "refresh_token",
            "client_id": self.client_id,
            "client_secret": self.client_secret,
            "refresh_token": self.refresh_token,
        }).encode()
        req = urllib.request.Request(
            ZOHO_OAUTH,
            data=payload,
            method="POST",
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )
        with urllib.request.urlopen(req, timeout=10) as resp:
            data = json.loads(resp.read().decode())
        token = data["access_token"]
        # Refresh 60s before expiry to avoid edge races.
        self._expires_at = time.time() + max(60, int(data.get("expires_in", 3600)) - 60)
        self._access_token = token
        log.info("refreshed access token (expires_in=%ss)", data.get("expires_in"))
        return token

    def access_token(self) -> str:
        with self._lock:
            if self._access_token and time.time() < self._expires_at:
                return self._access_token
            return self._refresh()

    def react_to_message(self, chat_id: str, message_id: str, emoji_code: str) -> bool:
        """Add a reaction to a message. Note: reaction shows as OAuth user, NOT bot."""
        if not self.configured:
            log.error("cliq_client not configured")
            return False
        url = (
            f"{CLIQ_API}/chats/{chat_id}/messages/"
            f"{urllib.parse.quote(message_id, safe='')}/reactions"
        )
        token = self.access_token()
        req = urllib.request.Request(
            url,
            data=json.dumps({"emoji_code": emoji_code}).encode(),
            method="POST",
            headers={
                "Authorization": f"Zoho-oauthtoken {token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                log.info("react(%s, %r): HTTP %d", message_id, emoji_code, resp.status)
                return resp.status in (200, 201, 204)
        except urllib.error.HTTPError as e:
            log.error("react failed: HTTP %d %s", e.code, e.read()[:200])
            return False

    def post_to_channel(
        self,
        channel_unique_name: str,
        text: str,
        reply_to: str | None = None,
    ) -> bool:
        """Post a message as the bot. If reply_to is set, render as a quote."""
        if not self.configured:
            log.error("cliq_client not configured — missing ZOHO_CLIQ_* env")
            return False
        url = (
            f"{CLIQ_API}/channelsbyname/{channel_unique_name}/message"
            f"?bot_unique_name={self.bot_name}"
        )
        payload: dict = {"text": text}
        if reply_to:
            payload["reply_to"] = reply_to
        token = self.access_token()
        req = urllib.request.Request(
            url,
            data=json.dumps(payload).encode(),
            method="POST",
            headers={
                "Authorization": f"Zoho-oauthtoken {token}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(req, timeout=10) as resp:
                log.info("post_to_channel(%s, reply_to=%s): HTTP %d",
                         channel_unique_name, reply_to, resp.status)
                return resp.status in (200, 201, 204)
        except urllib.error.HTTPError as e:
            log.error("post_to_channel(%s) failed: HTTP %d %s",
                      channel_unique_name, e.code, e.read()[:200])
            return False
        except Exception as e:
            log.error("post_to_channel(%s) error: %r", channel_unique_name, e)
            return False
