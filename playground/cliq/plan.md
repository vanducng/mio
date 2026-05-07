

# Zoho Cliq Bot Integration Plan

## What Works

### Authentication
- **OAuth Self Client** (for testing/dev)
  - Go to https://api-console.zoho.com/ -> Self Client
  - Generate grant token with scope, exchange for refresh token
  - Access tokens expire in 1hr, refresh tokens are permanent
- **Server-based Application** (recommended for production/Hatchet)
  - Go to https://api-console.zoho.com/ -> Add Client -> Server-based Applications
  - Set authorized redirect URI
  - More control: can set specific scopes, manage multiple tokens
  - Better for long-running server processes

### API Endpoint (Post as Bot to Channel)
```
POST https://cliq.zoho.com/api/v2/channelsbyname/{CHANNEL_NAME}/message?bot_unique_name={BOT_NAME}
```

### Headers
```
Authorization: Zoho-oauthtoken {ACCESS_TOKEN}
Content-Type: application/json
```

**IMPORTANT:** Auth header is `Zoho-oauthtoken`, NOT `Bearer`.

### Request Body
```json
{"text": "Your message here"}
```
- `text` is a string (per OpenAPI spec), max 5000 chars
- Optional: `reply_to` (message ID), `sync_message` (boolean)

### Working curl Example
```bash
curl -X POST "https://cliq.zoho.com/api/v2/channelsbyname/tobytime/message?bot_unique_name=tobytime" \
  -H "Authorization: Zoho-oauthtoken {ACCESS_TOKEN}" \
  -H "Content-Type: application/json" \
  -d '{"text":"Hello from Hatchet Bot!"}'
```

### Token Refresh (for automation)
```bash
curl -X POST "https://accounts.zoho.com/oauth/v2/token" \
  -d "grant_type=refresh_token" \
  -d "client_id={CLIENT_ID}" \
  -d "client_secret={CLIENT_SECRET}" \
  -d "refresh_token={REFRESH_TOKEN}"
```
Returns new `access_token` (1hr TTL). Refresh token doesn't change.

### Rate Limits
- Channel message: 50 req/min per user
- Lock period: 10 min (if exceeded)

## Credentials
- **Self Client** (dev/test): see previous conversation history
- **Server-based App** (production): stored in `secrets.env` (same directory)
- Scopes: `ZohoCliq.Channels.ALL,ZohoCliq.Messages.CREATE,ZohoCliq.Webhooks.CREATE`
- API Domain: `https://www.zohoapis.com` (US)
- Hatchet Server: `https://hatchet.abspectrumservices.org`

## Channel & Bot Info
- Channel unique name: `tobytime`
- Channel ID: `O1974166000016769009`
- Chat ID: `CT_1608777491535075383_637446511`
- Bot unique name: `tobytime`
- Bot API endpoint: `https://cliq.zoho.com/api/v2/bots/tobytime/message`
- Bot incoming webhook: `https://cliq.zoho.com/api/v2/bots/tobytime/incoming`

## Key Findings
1. `text` as array `["msg"]` works but renders raw array brackets in channel
2. Using `-d @file.json` (file-based body) works reliably; inline `-d '{...}'` sometimes has shell escaping issues
3. Without `?bot_unique_name=tobytime`, message posts as the OAuth user (human)
4. With `?bot_unique_name=tobytime`, message posts as the bot
5. Bot must be a participant in the channel

## Production Setup (Server-based Application)

### Why Server-based over Self Client
- Self Client grant tokens expire in 10 min (manual step each time)
- Server-based app uses redirect flow once, then refresh token forever
- Better audit trail and permission management
- Can be scoped to specific org/team

### Steps

**Step 1: Create Server-based Application**
1. Go to https://api-console.zoho.com/ -> **Add Client** -> **Server-based Applications**
2. Fill in:
   - **Client Name**: `Hatchet Data Platform`
   - **Homepage URL**: `https://hatchet.abspectrumservices.org`
   - **Authorized Redirect URI**: `https://hatchet.abspectrumservices.org/api/v1/users/google/callback`
     (or any URL you control - it just needs to be reachable to capture the auth code once)

3. Click **Create** -> note the **Client ID** and **Client Secret**

**Step 2: Generate Authorization Code (one-time, in browser)**
Open this URL in your browser (replace `{CLIENT_ID}` and `{REDIRECT_URI}`):
```
https://accounts.zoho.com/oauth/v2/auth?scope=ZohoCliq.Webhooks.CREATE,ZohoCliq.Channels.ALL,ZohoCliq.Messages.CREATE&client_id={CLIENT_ID}&response_type=code&access_type=offline&redirect_uri={REDIRECT_URI}&prompt=consent
```
- Log in with your Zoho account
- Authorize the app
- You'll be redirected to `{REDIRECT_URI}?code=XXXXX`
- Copy the `code` value from the URL bar (it expires in ~2 min)

**Step 3: Exchange for Refresh Token (one-time, in terminal)**
```bash
curl -X POST "https://accounts.zoho.com/oauth/v2/token" \
  -d "grant_type=authorization_code" \
  -d "client_id={CLIENT_ID}" \
  -d "client_secret={CLIENT_SECRET}" \
  -d "redirect_uri={REDIRECT_URI}" \
  -d "code={AUTH_CODE_FROM_STEP_2}"
```
Response:
```json
{
  "access_token": "1000.xxxx",
  "refresh_token": "1000.yyyy",
  "token_type": "Bearer",
  "expires_in": 3600
}
```
Save the `refresh_token` - it never expires and is used by Hatchet to auto-generate access tokens.

**Step 4: Add Env Vars to Hatchet Server**

Add these to the Hatchet worker's environment (docker-compose, k8s secret, etc.):
```env
ZOHO_CLIQ_CLIENT_ID=1000.xxxxx
ZOHO_CLIQ_CLIENT_SECRET=xxxxxxxx
ZOHO_CLIQ_REFRESH_TOKEN=1000.yyyy
ZOHO_CLIQ_BOT_NAME=tobytime
ZOHO_CLIQ_CHANNEL_NAME=tobytime
```

### Hatchet Integration Plan

**Target**: https://hatchet.abspectrumservices.org

**Python utility** (in SCI worker codebase):
1. `refresh_access_token()` - calls Zoho OAuth to get fresh access token
2. `post_to_channel(message, channel, bot_name)` - posts message as bot
3. Token caching - reuse access token within 1hr TTL

**Usage in Hatchet workflows**:
```python
from services.zoho_cliq import post_to_channel

# At end of any scheduled task
post_to_channel(
    message="Pipeline XYZ completed successfully at 2026-03-09 00:00 UTC",
    channel="tobytime",
    bot_name="tobytime"
)
```

**Notification scenarios**:
- Pipeline success/failure alerts
- Scheduled report completion
- Data sync status updates
- Error/exception notifications

## Scripts

All scripts are in `.temp/cliq-bot/`:

| Script | Purpose |
|--------|---------|
| `setup-zoho-cliq-oauth.sh` | One-time OAuth setup: opens browser auth URL, exchanges code for refresh token, saves to secrets.env |
| `test-zoho-cliq-send-message.sh` | Test/utility: refresh tokens, send messages, update SCI .env |
| `secrets.env` | Credentials store (not committed to git) |

### Quick Commands

```bash
cd .temp/cliq-bot

# 1. Complete OAuth setup (one-time)
./setup-zoho-cliq-oauth.sh

# 2. Test sending a message
./test-zoho-cliq-send-message.sh
./test-zoho-cliq-send-message.sh "Custom message here"

# 3. Get a fresh access token for manual curl testing
./test-zoho-cliq-send-message.sh refresh

# 4. Update SCI .env with Zoho Cliq vars (for local dev)
./test-zoho-cliq-send-message.sh update-sci-env

# 5. Manual curl test (after getting token from step 3)
curl -X POST "https://cliq.zoho.com/api/v2/channelsbyname/tobytime/message?bot_unique_name=tobytime" \
  -H "Authorization: Zoho-oauthtoken {ACCESS_TOKEN}" \
  -H "Content-Type: application/json" \
  -d @- <<< '{"text":"Manual test!"}'
```

### Validation Checklist
- [ ] Server-based App created at api-console.zoho.com
- [ ] OAuth flow completed (refresh token in secrets.env)
- [ ] Test message sent to #tobytime as bot
- [ ] SCI .env updated for local testing
- [ ] Env vars added to Hatchet worker deployment
- [ ] Python utility implemented in SCI backend
