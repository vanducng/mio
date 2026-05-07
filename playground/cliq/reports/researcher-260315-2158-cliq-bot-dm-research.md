# Zoho Cliq Bot DM Research Report

**Date:** 2026-03-15
**Research Questions:** How to send DMs as a bot in Zoho Cliq (not as OAuth token owner)

---

## Executive Summary

**Critical Finding:** Zoho Cliq does NOT provide a native mechanism to send direct messages as a bot identity separate from the OAuth token owner. All DM API calls (`/api/v2/buddies/{email}/message`) authenticate via the OAuth token holder's credentials, resulting in messages appearing to come from that user's account.

---

## Key Findings

### 1. Channel Messages Work as Bot (CONFIRMED)
- **Endpoint:** `POST /api/v2/channelsbyname/{channel}/message?bot_unique_name=tobytime`
- **Behavior:** Messages post as the bot, not the OAuth token owner
- **Why it works:** The `bot_unique_name` parameter explicitly specifies bot identity in channel context

### 2. Direct Message API (DMs via buddies endpoint)
- **Endpoint:** `POST /api/v2/buddies/{email}/message`
- **Authentication:** OAuth token (Zoho-oauthtoken header)
- **Current Behavior:** Messages appear to come from the OAuth token holder's account, NOT the bot
- **Bot Parameter Support:** The `bot_unique_name` query parameter is NOT accepted on this endpoint (returns "extra param" error, as you've confirmed)
- **Message Body Support:** Can include `bot` object with `name` and `image` properties in the message payload, but this only customizes the visual appearance within the message—it does NOT change the sender identity

### 3. Deluge Task Alternative
- **Task:** `zoho.cliq.postToUser("<email>", <message>, <connection>)`
- **Syntax:** `response = zoho.cliq.postToUser("user@example.com",{"text":"Hello","bot":{"name":"BotName","image":"<url>"}})`
- **Same Limitation:** Still sends as the connection owner (OAuth token holder), not as an independent bot account
- **Bot Parameter:** Only customizes message appearance, not sender identity

### 4. Bot-to-Bot Messaging (Different Pattern)
- **Endpoint:** `POST /api/v2/bots/{bot_unique_name}/message`
- **Target:** Other bots (subscribed users of a bot)
- **Parameters:** Can specify `userids` to target specific users who are subscribed to the bot
- **Use Case:** For bot-to-user broadcasts within bot handler workflows, NOT for arbitrary user DMs

### 5. OAuth Scope Limitations
- **Current Scopes:** `ZohoCliq.Webhooks.CREATE`, `ZohoCliq.Channels.ALL`, `ZohoCliq.Messages.CREATE`
- **Buddies Scope:** Operations on user chats require "Buddies" scope (not explicitly listed in your current scopes)
- **No Bot DM Scope:** No evidence of an additional OAuth scope for bot DM sending as a separate identity

---

## Architectural Constraint

Zoho Cliq treats bots as **handlers/integrations, not full user accounts**. Key implications:

- Bots have no independent mailbox or credential system
- Bots can only act through OAuth tokens held by actual users
- The "bot identity" is a **display customization layer**, not an authentication identity
- Channels support `bot_unique_name` because they're public spaces; DMs are tied to account ownership

---

## API Endpoint Summary

| Use Case | Endpoint | Supports bot_unique_name? | Sender Identity |
|----------|----------|-------------------------|-----------------|
| Channel message | `/api/v2/channelsbyname/{channel}/message` | ✓ Yes (query param) | Bot |
| Direct message | `/api/v2/buddies/{email}/message` | ✗ No (rejected as extra param) | OAuth token owner |
| Bot message to subscribers | `/api/v2/bots/{bot_unique_name}/message` | N/A (endpoint itself) | Bot (for subscribers) |
| Deluge postToUser | `zoho.cliq.postToUser()` | N/A | OAuth connection owner |

---

## Alternative Approaches Evaluated

### 1. Bot Incoming Webhook Handler
- **Purpose:** External systems POST messages to bot's webhook URL
- **Limitation:** Designed for receiving messages FROM external sources, not for bot-initiated DMs to users
- **Not applicable for solving this problem**

### 2. Bot Message Handler
- **Purpose:** Bot creator posts messages to subscribed users
- **Limitation:** Requires users to be subscribed to the bot; targets subscribers, not arbitrary users
- **Partial solution:** Could work for broadcast scenarios, but not for arbitrary user targeting

### 3. Bot Call Handler / Context Handler
- **Purpose:** Interactive command handlers, conversational flows
- **Limitation:** Triggered by user interaction, not bot-initiated; tied to channel/chat context
- **Not applicable for sending unsolicited DMs**

### 4. Webhook Token Authentication
- **Purpose:** Authenticate incoming webhooks from external services
- **Limitation:** Different use case; doesn't solve outbound DM sending as bot
- **Not applicable**

---

## Architecture Diagram

```
Zoho Cliq System Architecture (Messaging Identity)

┌─────────────────────────────────────────────────────────┐
│ OAuth User Account (duc@yds.services)                   │
│ ├─ OAuth Token (Zoho-oauthtoken header)                 │
│ └─ API Credentials                                      │
└──────────────┬──────────────────────────────────────────┘
               │
         ┌─────┴──────┐
         │             │
    ┌────▼────┐   ┌───▼─────┐
    │ Channel │   │   DM     │
    │ Messages│   │ Messages │
    └────┬────┘   └───┬─────┘
         │             │
    ┌────▼────┐   ┌───▼──────────┐
    │Bot Name │   │OAuth Owner    │
    │(display)│   │Identity       │
    └─────────┘   └───────────────┘

Bot Handler (Separate)
├─ bot_unique_name: "tobytime"
├─ Display Name, Image
├─ Subscribed Users List
├─ Can post to channels ✓
└─ Cannot post DMs as itself ✗
   (Only through OAuth token owner)
```

---

## Unresolved Questions

1. **Can Zoho Cliq bot accounts be created with separate credentials?** (No evidence found; appears bots are UI/handler constructs only)
2. **Is there a "bot user" account type distinct from regular users?** (Documentation suggests no; bots are handlers, not accounts)
3. **Does Zoho Cliq have a "service account" or "app account" feature for bots?** (Not found in current API/platform docs)
4. **Are there undocumented API headers (e.g., `X-Bot-Identity`, `X-Sender-Type`) to override DM sender identity?** (No evidence; API rejects `bot_unique_name` on buddies endpoint)
5. **Can custom Deluge scripts or bot handlers modify sender identity in DMs?** (Deluge `postToUser()` only supports `bot` display customization, not identity override)

---

## Recommendation

**For the "TobyTime" daily greeting use case:**

### Option A: Post to Channel (Current Working Approach)
- Post morning greeting to `#TobyTime` channel with bot identity
- Advantages: Works as intended, bot is visible as sender
- Disadvantages: Less personal than DMs, channel members see the message

### Option B: Broadcast via Bot Subscription (If Applicable)
- Make users subscribe to "tobytime" bot
- Use `POST /api/v2/bots/tobytime/message` with `userids` targeting all subscribed users
- Requires users to "follow" the bot first
- Advantages: Bot identity is preserved, targeted delivery
- Disadvantages: Requires bot subscription model

### Option C: Accept OAuth Owner Identity
- Continue using `/api/v2/buddies/{email}/message` or Deluge `postToUser()`
- Accept that DMs appear to come from duc@yds.services
- Use `bot.name` and `bot.image` in message payload to visually brand as "TobyTime"
- Advantages: Works today, no architectural changes needed
- Disadvantages: Misleading sender (says duc, displays as TobyTime)

### Option D: Request Bot Account Feature
- File feature request with Zoho Cliq for bot user accounts or service accounts
- Would enable true bot identity for DMs
- Timeline: Unknown, may take months

---

## Sources

- [Zoho Cliq REST API v2 Documentation](https://www.zoho.com/cliq/help/restapi/v2/)
- [Post to User API | Cliq](https://www.zoho.com/cliq/help/platform/post-to-user.html)
- [Post to User | Zoho Deluge](https://www.zoho.com/deluge/help/cliq/post-to-user.html)
- [Post to Bot | Cliq](https://www.zoho.com/cliq/help/platform/post-to-bot.html)
- [Bots | Cliq](https://www.zoho.com/cliq/help/platform/bots.html)
- [Developers FAQ | Cliq](https://www.zoho.com/cliq/help/platform/faq.html)
- [Connections | Cliq](https://www.zoho.com/cliq/help/platform/connections.html)

