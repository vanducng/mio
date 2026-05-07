"""Quick test: find user and send DM via Zoho Cliq bot."""

import asyncio
import os
import sys

# Load secrets from .env
from dotenv import load_dotenv

load_dotenv(os.path.join(os.path.dirname(__file__), "secrets.env"))

# Also try sci .env
sci_env = os.path.join(os.path.dirname(__file__), "../../services/sci/.env")
load_dotenv(sci_env, override=False)

import httpx

# Debug: check env loaded
for var in ["ZOHO_CLIQ_CLIENT_ID", "ZOHO_CLIQ_CLIENT_SECRET", "ZOHO_CLIQ_REFRESH_TOKEN"]:
    val = os.getenv(var, "")
    print(f"  {var}: {'set (' + val[:8] + '...)' if val else 'MISSING'}")


async def get_token() -> str:
    async with httpx.AsyncClient(timeout=30) as client:
        resp = await client.post(
            "https://accounts.zoho.com/oauth/v2/token",
            data={
                "grant_type": "refresh_token",
                "client_id": os.getenv("ZOHO_CLIQ_CLIENT_ID"),
                "client_secret": os.getenv("ZOHO_CLIQ_CLIENT_SECRET"),
                "refresh_token": os.getenv("ZOHO_CLIQ_REFRESH_TOKEN"),
            },
        )
        resp.raise_for_status()
        data = resp.json()
        if "access_token" not in data:
            print(f"Token refresh failed: {data}")
            sys.exit(1)
        return data["access_token"]


async def main():
    token = await get_token()
    headers = {
        "Authorization": f"Zoho-oauthtoken {token}",
        "Content-Type": "application/json",
    }
    bot_name = os.getenv("ZOHO_CLIQ_BOT_NAME", "tobytime")

    async with httpx.AsyncClient() as client:
        # Step 1: Try multiple endpoints to find users
        for endpoint in [
            "https://cliq.zoho.com/api/v2/buddies",
            "https://cliq.zoho.com/api/v2/contacts",
        ]:
            print(f"--- GET {endpoint} ---")
            resp = await client.get(endpoint, headers=headers)
            print(f"Status: {resp.status_code}")
            if resp.status_code == 200:
                data = resp.json()
                items = data.get("data", data)
                if isinstance(items, list):
                    for u in items[:20]:
                        print(f"  {u}")
                elif isinstance(items, dict):
                    for k, v in list(items.items())[:20]:
                        print(f"  {k}: {v}")
                else:
                    print(f"  {str(data)[:500]}")
            else:
                print(f"  {resp.text[:300]}")
            print()

        # Step 2: Try sending DM if recipient provided
        recipient = sys.argv[1] if len(sys.argv) > 1 else None
        if recipient:
            print(f"\n--- Sending test DM to {recipient} ---")
            resp = await client.post(
                f"https://cliq.zoho.com/api/v2/buddies/{recipient}/message",
                params={"bot_unique_name": bot_name},
                headers=headers,
                json={"text": "Hello! This is a test DM from TobyTime bot 🤖"},
            )
            print(f"Status: {resp.status_code}")
            print(f"Response: {resp.text[:500]}")
        else:
            print("\nTo send a test DM, run: python test-dm.py <email_or_zuid>")


asyncio.run(main())
