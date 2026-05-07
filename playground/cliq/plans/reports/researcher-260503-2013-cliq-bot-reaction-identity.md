# Research: Can Zoho Cliq attribute a reaction to a bot identity?

_Date: 2026-05-03 · Mode: default · Queries: 5_

## TL;DR
- **No.** Cliq has no documented mechanism to add a reaction as a bot. The reaction always appears authored by the OAuth token holder.
- This is a **Cliq architectural limit**, identical in shape to the prior finding that bots cannot send DMs as themselves — only channel posts via `?bot_unique_name=` switch the sender identity.

## What I checked

| Vector | Result |
|---|---|
| `?bot_unique_name=` query param on `POST /chats/{cid}/messages/{mid}/reactions` | Not documented. Reactions docs (Add / Get / Delete) make no mention of bot identity at all. ([REST API v2](https://www.zoho.com/cliq/help/restapi/v2/)) |
| Handler **response Map** keys (Participation, Mention, Message, Message-Action) | Six supported response types: *Messages (card+buttons)*, *Banner*, *Form*, *Bot Suggestions*, *Bot Context*, *Message Edit*. **No "react", "reaction", "emoji_code", or "add_reaction" key**. ([Cliq Responses](https://www.zoho.com/cliq/help/platform/cliq-responses.html)) |
| Bot-targeted endpoints (`/api/v2/bots/{name}/...`) | Only `message` and `incoming` — no `reaction` variant. ([Bots](https://www.zoho.com/cliq/help/platform/bots.html), [Post to Bot](https://www.zoho.com/cliq/help/platform/post-to-bot.html)) |
| Deluge `zoho.cliq.*` task list | Has `postToChannel`, `postToUser`, `postToBot`, `editMessage`. **No `addReaction` / `react`** task documented. ([Deluge: Cliq](https://www.zoho.com/deluge/help/cliq/zoho-cliq-integration-attributes.html)) |
| n8n community node `phil-fetchski/n8n-nodes-zoho-cliq` (production-validated) | `reaction.add.operation.ts` calls `POST /chats/{cid}/messages/{mid}/reactions` plain — no bot-identity field. The only sender-identity switch in the entire node is `bot_unique_name` on **channel message posts**. ([scopeRegistry.ts](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/nodes/ZohoCliq/v1/helpers/scopeRegistry.ts)) |

## Verdict

The OAuth user authoring the reaction is **expected behavior**, not a misuse. Zoho positions bots as **handlers/integrations**, not full identities; "bot identity" is a per-endpoint display layer that Cliq exposes only on the channel-message endpoint via `bot_unique_name`. Reactions, DMs, and message edits all run as the OAuth user.

## Workarounds (none make the reaction the bot)

1. **Reply with an emoji as text** — bot can post `👀` as a quoted reply via channel-message; this reads as "TobyTime: 👀" rather than a reaction icon under the original message. Closest UX with a true bot identity.
2. **Use a service account** that is named "TobyTime Bot" — the OAuth token holder's display name then *looks* bot-like. Not a real fix; reaction is still attributed to that human user.
3. **File a feature request** with Zoho Cliq for `bot_unique_name` on reactions. No public ETA exists.

## Open questions

- Whether undocumented headers (e.g. `X-Cliq-Bot-Identity`) exist — only Zoho support could confirm.
- Whether Zoho is planning to add bot-as-reactor support; no roadmap signal found.
