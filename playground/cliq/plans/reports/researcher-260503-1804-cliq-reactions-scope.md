# Research: Zoho Cliq reactions — correct OAuth scope + emoji_code format

_Date: 2026-05-03 · Mode: default · Queries: 5_

## TL;DR
- **Right scope:** `ZohoCliq.Messages.UPDATE` (one scope governs add **and** remove reactions; `ZohoCliq.Messages.READ` covers GET).
- **Wrong scope (the one Zoho rejected):** `ZohoCliq.Reactions.*` — **does not exist** as a top-level resource. There is no `ZohoCliq.Reactions.CREATE` or `.UPDATE`.
- **`emoji_code` accepts BOTH**: a Unicode glyph (`👍`) **or** a Cliq shortcode (`:smile:`, `:thumbsup:`). Send either string verbatim in the body.

## Authoritative source

The `phil-fetchski/n8n-nodes-zoho-cliq` community node ships a `DEFAULT_CLIQ_SCOPES` constant that's been validated against Zoho's consent flow in production. Relevant entries:

```ts
export const DEFAULT_CLIQ_SCOPES = [
  'ZohoCliq.Messages.READ',     // GET reactions
  'ZohoCliq.Messages.UPDATE',   // POST/DELETE reactions ← THIS is what we need
  'ZohoCliq.Messages.DELETE',
  'ZohoCliq.messages.CREATE',   // post messages (also accepted as 'ZohoCliq.Messages.CREATE')
  'ZohoCliq.OrganizationMessages.READ',
  ...
];
```
([source](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/credentials/ZohoCliqOAuth2Api.credentials.ts))

The reactions handlers (`reaction/add.operation.ts`, `get.operation.ts`, `remove.operation.ts`) all run under this same scope set — confirming `ZohoCliq.Messages.UPDATE` is the gate, not a separate `Reactions.*` namespace.

## emoji_code format (from n8n's tool-description for the operation)

> "Required emoji to add as the reaction. Send either a **real Unicode emoji** such as 👍 or a **known Zoho Cliq shortcode** such as `:smile:`. Do not leave blank."
> — [`AiAgentToolDescriptions/Resources/Reaction/add.md`](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/AiAgentToolDescriptions/Resources/Reaction/add.md)

GET reactions returns a `data` object whose keys are the same forms (mixed unicode + shortcode). For idempotent remove, reuse the exact key returned.

## Fix for our setup script

Replace `ZohoCliq.Reactions.UPDATE` (invalid) with nothing — `ZohoCliq.Messages.UPDATE` already covers it. Final scope CSV for reactions support:

```
ZohoCliq.Webhooks.CREATE,ZohoCliq.Channels.ALL,ZohoCliq.Messages.CREATE,ZohoCliq.Chats.READ,ZohoCliq.Channels.READ,ZohoCliq.Messages.READ,ZohoCliq.Messages.UPDATE,ZohoCliq.OrganizationMessages.READ
```

## Bonus scopes worth noting (for future work)

- `ZohoCliq.Messages.DELETE` — bot self-delete its own messages
- `ZohoCliq.Attachments.READ` — fetch attachment file bytes
- `ZohoCliq.messageactions.{READ,CREATE,DELETE}` — message action handlers (note: Zoho's spelling here uses lowercase `messageactions`)

## Open questions

- Whether `ZohoCliq.Messages.CREATE` (capital M) and `ZohoCliq.messages.CREATE` (lowercase) are both accepted, or if Zoho's parser is case-insensitive — both appear in working code samples; safe to keep capital.

## References
- [n8n credentials source — DEFAULT_CLIQ_SCOPES](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/credentials/ZohoCliqOAuth2Api.credentials.ts)
- [n8n Reaction add tool description](https://github.com/phil-fetchski/n8n-nodes-zoho-cliq/blob/main/AiAgentToolDescriptions/Resources/Reaction/add.md)
- [Zoho Cliq REST API v2 (Reactions section)](https://www.zoho.com/cliq/help/restapi/v2/#Get_Reaction)
- [Zoho Cliq Postman collection](https://www.postman.com/cliqgeeks/zoho-cliq/collection/8s2nyyh/zoho-cliq-rest-apis-v2)
