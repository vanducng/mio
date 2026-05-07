# Zoho Cliq Hatchet Integration - Implementation Plan

## Context
- Zoho Cliq OAuth (Server-based App) is working
- Bot `tobytime` can post to `#TobyTime` channel
- Credentials in `.temp/cliq-bot/secrets.env`
- SCI .env updated with Zoho Cliq vars
- Existing Hatchet tasks pattern: `services/sci/backend/app/tasks/{module}/`
- Worker entrypoint: `services/sci/backend/app/tasks/worker.py`

## Credentials (in secrets.env)
```
ZOHO_CLIQ_CLIENT_ID=1000.ZUGGSQ5O54D3CVF9U1DR4UIU797APS
ZOHO_CLIQ_CLIENT_SECRET=3507b6f0c84f1e63b51d580f7f324f78caffa15334
ZOHO_CLIQ_REFRESH_TOKEN=(stored in secrets.env after running setup script) @.temp/cliq-bot/secrets.env
ZOHO_CLIQ_BOT_NAME=tobytime
ZOHO_CLIQ_CHANNEL_NAME=tobytime
ZOHO_CLIQ_REDIRECT_URI=http://localhost:8080
```

## API Reference
- **Endpoint**: `POST https://cliq.zoho.com/api/v2/channelsbyname/{channel}/message?bot_unique_name={bot}`
- **Auth header**: `Authorization: Zoho-oauthtoken {ACCESS_TOKEN}` (NOT Bearer)
- **Body**: `{"text": "message"}` (string, max 5000 chars) — use `-d @file.json` to avoid shell escaping
- **Token refresh**: `POST https://accounts.zoho.com/oauth/v2/token` with `grant_type=refresh_token`
- **Rate limit**: 50 req/min per user, 10 min lock if exceeded

## Implementation Plan

### Phase 1: Cliq Client Module
**Files to create:**
- `services/sci/backend/app/tasks/cliq/__init__.py` (already exists, empty)
- `services/sci/backend/app/tasks/cliq/client.py` — OAuth token refresh + send message

**client.py pattern:**
```python
import os
import time
import httpx
import structlog

logger = structlog.get_logger(__name__)

# Token cache
_token_cache = {"access_token": None, "expires_at": 0}

async def refresh_access_token() -> str:
    """Get fresh access token, using cache if valid."""
    if _token_cache["access_token"] and time.time() < _token_cache["expires_at"]:
        return _token_cache["access_token"]

    async with httpx.AsyncClient() as client:
        resp = await client.post("https://accounts.zoho.com/oauth/v2/token", data={
            "grant_type": "refresh_token",
            "client_id": os.getenv("ZOHO_CLIQ_CLIENT_ID"),
            "client_secret": os.getenv("ZOHO_CLIQ_CLIENT_SECRET"),
            "refresh_token": os.getenv("ZOHO_CLIQ_REFRESH_TOKEN"),
        })
        data = resp.json()
        _token_cache["access_token"] = data["access_token"]
        _token_cache["expires_at"] = time.time() + data.get("expires_in", 3600) - 60
        return data["access_token"]

async def post_to_channel(message: str, channel: str = None, bot_name: str = None) -> bool:
    """Post message to Zoho Cliq channel as bot."""
    channel = channel or os.getenv("ZOHO_CLIQ_CHANNEL_NAME", "tobytime")
    bot_name = bot_name or os.getenv("ZOHO_CLIQ_BOT_NAME", "tobytime")
    token = await refresh_access_token()

    url = f"https://cliq.zoho.com/api/v2/channelsbyname/{channel}/message"
    async with httpx.AsyncClient() as client:
        resp = await client.post(
            url,
            params={"bot_unique_name": bot_name},
            headers={
                "Authorization": f"Zoho-oauthtoken {token}",
                "Content-Type": "application/json",
            },
            json={"text": message},
        )
        # 204 = success (no content)
        success = resp.status_code in (200, 204) or resp.content == b""
        logger.info("cliq_message_sent", channel=channel, bot=bot_name, success=success)
        return success
```

### Phase 2: Daily Greeting Workflow (Test Task)
**File:** `services/sci/backend/app/tasks/cliq/daily_greeting.py`

```python
"""Daily greeting workflow — posts to #TobyTime at 7:25 AM ET."""
import os
from datetime import datetime, timedelta
from hatchet_sdk import Context, EmptyModel, Hatchet
import structlog
from app.tasks.cliq.client import post_to_channel

logger = structlog.get_logger(__name__)
hatchet = Hatchet()

APP_ENV = os.getenv("APP_ENV", "local")
# 7:25 AM ET = 12:25 UTC (ET is UTC-5, or UTC-4 in DST)
CRON_SCHEDULE = ["25 12 * * *"] if APP_ENV == "prod" else []

daily_greeting_workflow = hatchet.workflow(
    name="cliq-daily-greeting",
    on_crons=CRON_SCHEDULE,
    default_priority=1,
)

@daily_greeting_workflow.task(
    name="send-greeting",
    execution_timeout=timedelta(seconds=30),
    retries=2,
)
async def send_greeting(input: EmptyModel, ctx: Context) -> dict:
    now = datetime.now().strftime("%A, %B %d %Y")
    message = f"Good morning! Today is {now}. All systems operational."
    success = await post_to_channel(message)
    return {"sent": success, "message": message}
```

### Phase 3: Register in Worker
**File to edit:** `services/sci/backend/app/tasks/worker.py`

Add import:
```python
from app.tasks.cliq.daily_greeting import daily_greeting_workflow
```

Add to workflows list:
```python
workflows = [
    sync_workflow,
    batch_sync_workflow,
    reminder_workflow,
    monthly_hours_workflow,
    missing_email_alert_workflow,
    daily_greeting_workflow,  # NEW
]
```

### Phase 4: Update .env files
**services/sci/.env** — add (if not already):
```env
ZOHO_CLIQ_CLIENT_ID=<from secrets.env>
ZOHO_CLIQ_CLIENT_SECRET=<from secrets.env>
ZOHO_CLIQ_REFRESH_TOKEN=<from secrets.env>
ZOHO_CLIQ_BOT_NAME=tobytime
ZOHO_CLIQ_CHANNEL_NAME=tobytime
```

**services/sci/.env.example** — already updated with empty placeholders.

### Phase 5: Testing

**Local test (manual trigger via Hatchet UI):**
1. `cd services/sci && docker compose up`
2. Open https://hatchet.dev.abspectrumservices.org (or local)
3. Find `cliq-daily-greeting` workflow
4. Click "Trigger" to run manually
5. Check #TobyTime channel for message

**CLI test:**
```bash
# Quick test with the shell script
cd .temp/cliq-bot
./test-zoho-cliq-send-message.sh "Manual test from CLI"

# Python test inside container
docker compose exec backend python -c "
import asyncio
from app.tasks.cliq.client import post_to_channel
asyncio.run(post_to_channel('Test from Python!'))
"
```

## File Summary

| Action | File |
|--------|------|
| Create | `services/sci/backend/app/tasks/cliq/client.py` |
| Create | `services/sci/backend/app/tasks/cliq/daily_greeting.py` |
| Edit | `services/sci/backend/app/tasks/worker.py` (add import + workflow) |
| Verify | `services/sci/.env` (Zoho Cliq vars present) |
| Already done | `services/sci/.env.example` (placeholders added) |

## Checklist
- [ ] Create client.py with token refresh + post_to_channel
- [ ] Create daily_greeting.py workflow (7:25 AM ET cron)
- [ ] Register workflow in worker.py
- [ ] Verify .env has credentials
- [ ] Test locally via Hatchet UI manual trigger
- [ ] Test message appears in #TobyTime as bot
- [ ] Deploy to prod
