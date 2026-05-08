# Research: Channels Data Model for MIO

_Date: 2025-05-07 · Mode: --deep · Queries: 11_
_Author: vd:research · Scope: P1 proto envelope + future-proofing_

---

## TL;DR

- **Recommendation: a four-tier polymorphic model — `Tenant → Account → Conversation (kind) → Message` — with threading expressed as a Matrix-style `relates_to` reference rather than a separate hierarchy.** Conversation is one table with a `kind` discriminator (`dm | group_dm | channel_public | channel_private | thread | forum_post | broadcast`), addressed by an opaque `external_id` + `external_parent_id` pair and a stable internal UUID v7.
- **Runner-up: Mattermost/Sendbird-style "single channel table, type column"** without explicit `account` (workspace) tier — wins if mio commits to one install per tenant per channel-type, which is unrealistic the moment a customer has two Slack workspaces.
- **Avoid: per-channel-type tables** (one table for Slack channels, one for Discord guilds, etc.). Every popular project that tried it converged back to a single polymorphic table within 18 months. Hard to query across, painful to add new channels, kills the "agents shouldn't care" thesis.
- **Hardest lesson from goclaw**: `tenant_id` retrofit (migration 27, ~30 ALTERs), `thread_id` retrofit (migration 35), and `channel_type` rename via tmp-sentinel swap (migration 58). All three are baked into the recommendation here so mio doesn't repeat them.

---

## 1. The Question

How should mio model "where a message lives" in its canonical envelope so that:

1. The Cliq POC works without leaking Cliq-isms into the schema.
2. Slack / Telegram / Discord / WhatsApp / Zalo / Feishu can be added as one-day adapter jobs (the litmus test in `plan.md` P9).
3. Multi-tenant isolation is real from day 1 — not a migration-27 retrofit.
4. Threads, group-DMs, forum posts, broadcast channels, ephemeral 1-on-1s all fit one envelope.
5. The proto schema can outlive its first six adapters without a v2 break.

This is the schema we're locking before SDKs and gateway code touch it. The cost of getting it wrong is `goclaw/migrations/000027` × 30 tables.

---

## 2. Evaluation Criteria

- **Extensibility** — adding a new channel type or kind without breaking proto consumers
- **Tenant isolation** — first-class, indexed, present on every row
- **Conversation polymorphism** — DM, group-DM, public/private channel, thread, forum post, broadcast all addressable uniformly
- **Threading expressivity** — slack-`thread_ts`, discord parent_id, matrix m.thread, zulip topic, mattermost RootId all map cleanly
- **Identity model** — external sender → canonical user, cross-channel merge possible
- **Idempotency-friendly** — `(channel_type, external_id)` is a stable composite key
- **Storage cost / write amplification** — single-table polymorphism vs joined tier
- **Schema evolution** — proto-buf compatible (forward + backward), enum vs string for `channel_type`
- **Federation-readiness** — does the model accommodate Matrix/ActivityPub-style federation later?
- **Lock-in / reversibility** — could mio swap NATS for Pub/Sub or Postgres for Spanner without re-modelling?

---

## 3. Reference: goclaw — what worked, what bled

goclaw is mio's closest sibling. Its scars are the cheapest data we have.

### What goclaw got right

| Decision | Where | Why it survived |
|---|---|---|
| `channel_type` (platform) ≠ `channel_instance` (this install) | `BaseChannel.Type()` vs `Name()` (channel.go:217) | Lets one tenant run two Slack workspaces or two Telegram bots without colliding |
| `peer_kind ∈ {direct, group}` on every inbound | `bus/types.go:27` | One field, two policy paths (DMPolicy / GroupPolicy in channel.go:55-72), trivial routing |
| Opaque `chat_id` not parsed by gateway | `bus/types.go:23` | Channel-specific composition (Cliq `chat_id`+`bot_unique_name`, Slack `channel_ts`, TG `chat_id`+`message_thread_id`) hidden behind one string |
| `Metadata map[string]string` escape hatch | `bus/types.go:33` | Every weird channel-specific quirk lands here without proto changes |
| Contact auto-collection (`channel_contacts`) | migration 14 | Built up canonical user table from message traffic without an explicit signup |
| `merged_id` for cross-channel identity merge | migration 14 | Same human across Slack + Telegram becomes one mio user later |

### What bled

| Pain | Migration | Cost | Mio fix |
|---|---|---|---|
| `tenant_id` retrofit | 27 | ALTER on ~30 tables, default-then-drop dance, every UNIQUE constraint rewritten | **Day 1: every table + every proto message has `tenant_id`.** No exceptions, no `'.'` workspace strings, no nullable-then-tighten. |
| `thread_id` added to contacts | 35 | Cleanup of `sender_id="123\|username"` rows + UNIQUE rebuild | **Day 1: `thread_id` and `parent_conversation_id` are core proto fields, even when null.** |
| `channel_type` rename (zalo_oa ↔ zalo_bot) | 58 | Three-step UPDATE swap via tmp sentinel | **Pin channel_type strings via a registry.** Renames go through alias-then-deprecate, never UPDATE-in-place. |
| `session_key` deprecated mid-flight | comment in `bus/types.go:26` | Gateway-built canonical key replaces serialized field | **Don't put computed keys in the wire envelope.** Session/conversation keys derive at the consumer from `(tenant_id, account_id, conversation_id)`. |
| `channel` field used as both instance name and platform type in places | gateway routing | Ambiguity in policy and outbound routing | **Wire envelope has both `channel_type` (enum-like string) and `account_id` (UUID).** Never overload one field. |
| Channel-pending-messages history | migration 12 | Group-chat compaction needed retrofitted table | **Plan compaction storage at envelope time** — a `parent_message_id` and `is_summary` flag belong in the message proto, not bolted on later. |

### The big-picture lesson

**Goclaw started as "one bot, one channel, one user" and grew tenants, threads, multi-account, identity merge in that order.** Each was a migration. Mio's bet — that messaging is *the* product, not a side feature — means all four must be present from the first proto generation. Cheap now, expensive later is literally what every goclaw migration after #10 demonstrates.

---

## 4. Industry Data Models — compact survey

Each platform's "where a message lives" decomposed.

| Platform | Tenant tier | Account tier | Conversation tier | Type discriminator | Thread mechanic |
|---|---|---|---|---|---|
| **Slack** | Enterprise Grid (org) | Workspace (`T…`) | `conversations` (one table for all) | `is_channel / is_group / is_im / is_mpim / is_private` booleans | `thread_ts` = parent message's `ts`; reply has both `ts` and `thread_ts` |
| **Discord** | — (no real tenant) | Guild (`guild_id`) | `channels` table + `ChannelType` enum (24 values incl. GUILD_TEXT, DM, GROUP_DM, GUILD_VOICE, GUILD_FORUM, GUILD_MEDIA, PUBLIC_THREAD, PRIVATE_THREAD, ANNOUNCEMENT_THREAD) | enum int | Thread is a *channel* with `parent_id` pointing at the text channel; `owner_id` is creator; metadata in `thread_metadata` |
| **Mattermost** | — (single instance, license-gated) | Team (`team_id`) | `Channels` table, `Type ∈ {O,P,D,G}` (open/private/direct/group) | enum char | `Posts.RootId` points at thread root post in same channel |
| **Rocket.Chat** | — | — (single workspace) | `rocketchat_room`, `t ∈ {c,p,d,l,v}` (channel/private/direct/livechat/voice) | enum char | `messages.tmid` = thread message id |
| **Matrix** | Server (federated) | — (rooms span servers) | `rooms` (event log per room) | not a column — type carried in events: `m.room.message`, `m.room.encrypted`, `m.space`, `m.dm` (heuristic) | `m.relates_to: { rel_type: "m.thread", event_id: $root }` (MSC3440); thread roots also live in main timeline |
| **Zulip** | Realm | — | Stream (== channel) + Topic (per-message) | stream + topic both required | Topic *is* the thread; every message carries `(stream_id, topic)` |
| **Sendbird** | Application (tenant) | — | `OpenChannel` vs `GroupChannel` (two tables) | model-class | Threads via `parent_message_id` |
| **goclaw** | `tenants` (UUID v7) | `channel_instances` (per platform install) | implicit — `(channel_type, chat_id)` is the conversation key | `peer_kind ∈ {direct, group}` + `channel_type` string | retrofitted `thread_id` on contacts (migration 35) |

### Patterns that recur

1. **Single conversation table with a type discriminator** — Slack, Mattermost, Rocket.Chat, Discord, even Matrix-as-room-list. Sendbird's two-table split is the outlier and they regret the open/group divergence in their own SDK migration guides.
2. **Threading as a relation, not a sub-tree** — Matrix is the cleanest spec; Mattermost `RootId` and Rocket.Chat `tmid` and Slack `thread_ts` are the same idea: a message points at its root, computed views form trees. Discord is the outlier (thread = full-blown channel) and pays for it with thread cleanup logic, archival flags, auto-archive timers.
3. **Tenant tier above workspace tier** — explicit in Slack Enterprise Grid, Sendbird (application), Matrix (homeserver). Implicit-then-bolted-on in Mattermost, Rocket.Chat. The two that bolted it on later regret it.
4. **External ID + internal ID, never collapsed** — every platform keeps the source-system ID alongside its own. mio already plans this (`source_message_id` for idempotency); extend it to conversations + accounts.

---

## 5. Three options for mio's data model

### Option A — Per-channel-type tables

```
slack_channels(...)
discord_guilds(...)
discord_channels(...)
telegram_chats(...)
zoho_cliq_chats(...)
```

Each adapter writes its own table; `Message` carries a tagged union pointing at one of them.

**Strengths:** strong typing per platform; native columns for native quirks.
**Weaknesses:**
- Every cross-channel query is a UNION
- Adding a channel = schema migration + new SDK type + new bus subject + new dashboard
- "Agents shouldn't care" violated at the schema level — agents query a polymorphic interface but storage isn't
- **Goclaw evidence:** they didn't do this, and adding `feishu/discord/whatsapp/zalo` was config + adapter only — no schema work
**Dealbreaker:** kills mio's core thesis. Don't.

### Option B — Single polymorphic conversation table (RECOMMENDED)

```
tenants
  ↓
accounts                        (one row per tenant per channel install)
  ↓
conversations                   (one row per DM, group, channel, thread, forum post)
  ↓
messages                        (one row per message; edits = new row referencing original)
```

`conversation.kind` discriminates. `external_id` is opaque per channel. `parent_conversation_id` makes a thread/forum-post a *child* conversation rather than an inline column on messages — this aligns with Discord's reality (threads are channels) AND Slack's (thread_ts addressable as a conversation slice) AND Matrix's (thread root is an event with relations) without forcing one onto the others.

**Strengths:**
- One table, one query path, one index strategy
- New channel type = adapter code + zero schema
- Threads, forum posts, ephemeral DMs all share routing/permission/retention logic
- Maps cleanly to NATS subject grammar (one subject template, all conversations)
- Mattermost / Rocket.Chat / Slack-internal all run this way at multi-million-message scale
**Weaknesses:**
- Discriminator drift if `kind` enum isn't disciplined
- Tall-and-skinny indexes need composite `(tenant_id, account_id, kind, external_id)`
- JSONB `attributes` column tempts everyone to dump untyped data; needs review discipline
**Dealbreakers:** none for mio's scale (POC → ~thousands of conversations, not millions of rooms)

### Option C — Matrix-style event-sourced rooms

Every state change (room created, member joined, topic set, message sent) is an event in an append-only log, with state computed by replay.

**Strengths:** federation-ready; perfect audit trail; replay solves backfill
**Weaknesses:**
- Massively more complex than a CRUD model
- Storage 3-5× heavier (state events for every change)
- Query patterns require materialised views or constant replay
- Mio's NATS+Postgres bet doesn't natively support event sourcing — would need a separate state store
- Federation is **not** a goal stated in mio's architecture doc
**Dealbreaker:** complexity for hypothetical futures. Save for a v2 only if federation becomes real.

---

## 6. Comparison Matrix

| Criterion | A: per-type tables | **B: polymorphic conversation** | C: event-sourced rooms |
|---|---|---|---|
| Add channel = schema work? | yes (migration per channel) | **no (config + adapter)** | partial (event types per channel) |
| Cross-channel query | UNION nightmare | **single SELECT** | replay-or-projection |
| Tenant isolation | per-table FK | **single FK pattern, replicable** | event metadata |
| Thread expressivity | per-channel hack | **uniform via `parent_conversation_id`** | native (relations) |
| Schema evolution | proto break per channel | **proto stable; `kind` enum + `attributes` JSONB** | event versioning per type |
| Storage overhead | low | **low–medium** | high (3-5×) |
| Operational burden | high (N tables) | **medium (1 table, disciplined)** | high (event store + projections) |
| Query latency | fast per-channel, slow cross | **fast (composite index)** | slow without projections |
| Lock-in to RDBMS | loose | **loose** | tight to event store |
| Federation-ready | no | partial (add federation events later) | **yes** |
| Scale ceiling for POC | wrong shape | **right shape** | over-built |
| Goclaw lessons absorbed | none | **all 6** | partial |

---

## 7. Recommended schema sketch

### 7.1 Proto envelope (additions/changes vs current `plan.md` P1)

```proto
// mio/v1/account.proto
message Account {
  // mio-internal stable ID (UUID v7)
  string id = 1;
  // tenant scope
  string tenant_id = 2;
  // platform identifier (string-with-registry, NOT enum — see §8)
  string channel_type = 3;
  // human label, e.g. "Acme — Cliq", "Engineering Slack"
  string display_name = 4;
  // platform-side install ID (workspace ID, guild ID, bot user ID, ...)
  string external_id = 5;
  // adapter-specific config (oauth tokens are in vault, not here)
  map<string, string> attributes = 6;
  google.protobuf.Timestamp created_at = 7;
}

// mio/v1/conversation.proto
message Conversation {
  string id = 1;                      // mio UUID v7
  string tenant_id = 2;
  string account_id = 3;              // FK → Account
  string channel_type = 4;            // denormalized for routing without join
  ConversationKind kind = 5;          // see enum below
  string external_id = 6;             // opaque, channel-defined
  string parent_conversation_id = 7;  // empty unless this is a thread / forum post / sub-channel
  string parent_external_id = 8;      // mirrors parent_conversation_id at source layer
  string display_name = 9;            // empty for DMs
  bool is_archived = 10;
  map<string, string> attributes = 11;
  google.protobuf.Timestamp created_at = 12;
}

enum ConversationKind {
  CONVERSATION_KIND_UNSPECIFIED = 0;
  CONVERSATION_KIND_DM = 1;             // 1:1 direct message
  CONVERSATION_KIND_GROUP_DM = 2;       // multi-party DM, no formal channel
  CONVERSATION_KIND_CHANNEL_PUBLIC = 3; // public room/channel
  CONVERSATION_KIND_CHANNEL_PRIVATE = 4;// private room/channel
  CONVERSATION_KIND_THREAD = 5;         // sub-conversation under another conversation
  CONVERSATION_KIND_FORUM_POST = 6;     // thread that lives in a forum (Discord/Discourse-style)
  CONVERSATION_KIND_BROADCAST = 7;      // one-to-many announce/news/Telegram channel
}

// mio/v1/message.proto (revised from existing P1 plan)
message Message {
  string id = 1;                       // mio UUID v7
  string tenant_id = 2;
  string account_id = 3;
  string conversation_id = 4;          // FK — always set
  string thread_root_message_id = 5;   // empty if not in a thread (Matrix-style relation, not just a fk to Conversation)
  string source_message_id = 6;        // (channel_type + external_id are the dedup key with this)
  string external_conversation_id = 7; // denormalized for fast adapter use without join
  Sender sender = 8;
  string content = 9;
  repeated Attachment attachments = 10;
  MessageRelation relation = 11;       // edit_of / reply_to / reaction_to — same shape as Matrix m.relates_to
  bool is_summary = 12;                // for compaction-generated synthetic messages
  google.protobuf.Timestamp received_at = 13;
  google.protobuf.Timestamp sent_at = 14;
  map<string, string> attributes = 15;
}

message MessageRelation {
  enum Type {
    RELATION_UNSPECIFIED = 0;
    RELATION_REPLIES_TO = 1;
    RELATION_EDIT_OF = 2;
    RELATION_REACTION_TO = 3;
    RELATION_THREAD_REPLY = 4;
  }
  Type type = 1;
  string target_message_id = 2;        // mio ID
  string target_external_id = 3;       // source ID, for adapter resolution
}

message Sender {
  string external_id = 1;              // platform user ID
  string user_id = 2;                  // mio canonical user (may be empty for unmerged contacts)
  string display_name = 3;
  PeerKind peer_kind = 4;              // 'direct' | 'group' carried for legacy parity with goclaw
  bool is_bot = 5;
}
enum PeerKind { PEER_KIND_UNSPECIFIED=0; PEER_KIND_DIRECT=1; PEER_KIND_GROUP=2; }
```

Notes:
- `tenant_id` and `account_id` on **every** message — no implicit tenant. Mirrors goclaw migration 27 baked in upfront.
- `conversation_id` always set, even for DMs. The DM is just a `Conversation` with `kind=DM` and two members.
- `thread_root_message_id` is a parallel-track to `parent_conversation_id` on `Conversation`. The first lets a message belong to a Matrix-style thread without needing a separate Conversation row (cheap threads); the second models channels-as-threads (Discord-style). Both views compose.
- `attributes` JSONB at every level — proto v1 stable, channel-specific weirdness lands here, not in proto schema bumps.

### 7.2 Postgres schema sketch (operational store, lives in MIU but driven by this envelope)

```sql
CREATE TABLE tenants (
  id          UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  slug        VARCHAR(100) NOT NULL UNIQUE,
  status      VARCHAR(20)  NOT NULL DEFAULT 'active',
  created_at  TIMESTAMPTZ  NOT NULL DEFAULT NOW()
);

CREATE TABLE accounts (
  id           UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  tenant_id    UUID NOT NULL REFERENCES tenants(id),
  channel_type TEXT NOT NULL,                          -- registry-controlled string
  external_id  TEXT NOT NULL,                          -- platform install ID
  display_name TEXT NOT NULL,
  attributes   JSONB NOT NULL DEFAULT '{}',
  created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (tenant_id, channel_type, external_id)
);

CREATE TABLE conversations (
  id                       UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  tenant_id                UUID NOT NULL REFERENCES tenants(id),
  account_id               UUID NOT NULL REFERENCES accounts(id),
  channel_type             TEXT NOT NULL,              -- denormalized
  kind                     TEXT NOT NULL,              -- enum string
  external_id              TEXT NOT NULL,
  parent_conversation_id   UUID REFERENCES conversations(id),
  parent_external_id       TEXT,
  display_name             TEXT,
  is_archived              BOOLEAN NOT NULL DEFAULT false,
  attributes               JSONB NOT NULL DEFAULT '{}',
  created_at               TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (account_id, external_id)                     -- the idempotent address
);
CREATE INDEX ON conversations (tenant_id);
CREATE INDEX ON conversations (account_id, kind, is_archived);
CREATE INDEX ON conversations (parent_conversation_id) WHERE parent_conversation_id IS NOT NULL;

CREATE TABLE messages (
  id                       UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  tenant_id                UUID NOT NULL REFERENCES tenants(id),
  account_id               UUID NOT NULL REFERENCES accounts(id),
  conversation_id          UUID NOT NULL REFERENCES conversations(id),
  thread_root_message_id   UUID REFERENCES messages(id),
  source_message_id        TEXT NOT NULL,
  sender_external_id       TEXT NOT NULL,
  sender_user_id           UUID,                       -- nullable until contact merge
  content                  TEXT NOT NULL DEFAULT '',
  is_summary               BOOLEAN NOT NULL DEFAULT false,
  attributes               JSONB NOT NULL DEFAULT '{}',
  received_at              TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (account_id, source_message_id)               -- idempotency from edge
);
CREATE INDEX ON messages (tenant_id, conversation_id, received_at DESC);
CREATE INDEX ON messages (thread_root_message_id) WHERE thread_root_message_id IS NOT NULL;

CREATE TABLE channel_users (                           -- the goclaw 'channel_contacts' refresh
  id              UUID PRIMARY KEY DEFAULT uuid_generate_v7(),
  tenant_id       UUID NOT NULL REFERENCES tenants(id),
  account_id      UUID NOT NULL REFERENCES accounts(id),
  external_id     TEXT NOT NULL,                       -- per-platform user id
  user_id         UUID,                                -- canonical mio user (nullable)
  display_name    TEXT,
  attributes      JSONB NOT NULL DEFAULT '{}',
  first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (account_id, external_id)
);
```

Three things baked in that goclaw learned the hard way:
1. `tenant_id` on every table from row 1
2. `(account_id, external_id)` is the idempotent address — never `(channel_type, sender_id)` collapsed
3. `attributes JSONB` is the only legitimate place for channel-specific data; proto stays clean

### 7.3 NATS subject realignment

Current proposal in `system-architecture.md` §5:
```
mio.<direction>.<channel>.<workspace_id>.<thread_id>[.<message_id>]
```

Suggested adjustment to align tier names with the data model:
```
mio.<direction>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]
```

Why:
- "workspace" is overloaded (Slack uses workspace, Discord uses guild, Cliq uses team). `account_id` is mio's UUID and survives translation.
- `conversation_id` covers DM, group, public channel, *and thread* — the existing `thread_id` segment was actually doing thread-OR-conversation work and would have hit grammar gymnastics for non-thread DMs.
- `channel_type` (vs `channel` instance name) makes per-platform sender-pool consumers a clean filter — `mio.outbound.zoho_cliq.>` for the Cliq sender pool, `mio.outbound.slack.>` for Slack.
- `account_id` slots under `channel_type` for per-tenant rate-limit scoping just like the original intent.

This is a P1-time decision: cheaper to fix the subject grammar before the SDK ships than after.

---

## 8. `channel_type` — enum vs string with registry

The single most consequential extensibility decision.

| | Proto enum | String + Go/Py registry |
|---|---|---|
| Adding a channel | proto regen → all consumers redeploy | const + adapter ships, old SDKs ignore unknown |
| Typo protection | compile-time | unit-test-time |
| Forward compat | unknown enum value collapses to UNSPECIFIED | string flows through unchanged |
| Goclaw experience | n/a (used string) | 60 migrations, never had to break for a new channel |
| Vendor renames | painful (proto break or alias enum) | easy (alias in registry) |

**Recommendation: string + registry.** Proto enum looks safer at the REPL, but the migration cost when Zoho renames "cliq" or you add Lark-but-not-Feishu is real, and it forces a coordinated SDK redeploy across mio + miu. The registry pattern makes the gateway authoritative — adapters either know how to handle a `channel_type` or they don't, and the bus is transparent either way.

The registry should live in `proto/gen/go/...` as a generated `var KnownChannelTypes = map[string]bool{...}` from a single YAML in `proto/channels.yaml`. Both Go and Python read it; CI rejects PRs that introduce a `channel_type` not in the YAML.

---

## 9. Failure modes

| Mode | Symptom | Mitigation | Recovery cost |
|---|---|---|---|
| Conversation kind drift | `kind="THREAD"` vs `kind="thread"` vs `kind=5` across SDKs | Generate enum constants in both languages from one source; CI smoke-test each adapter for valid `kind` | Cheap if caught early; full table UPDATE if not |
| `attributes` JSONB land-grab | Untyped channel data leaks into core logic | Code-review rule: anything read by ≥2 callers gets promoted to a typed proto field with backfill | One JSONB→column migration |
| `external_id` collision across channel types | Two channels reuse the same opaque ID format | Composite key `(account_id, external_id)` not `(channel_type, external_id)` — already in schema sketch | Should never happen with composite |
| Thread root deletion | `thread_root_message_id` dangles | Soft-delete only on messages; mark `attributes.deleted=true`; never hard-delete | Free if soft-delete is policy from day 1 |
| Cross-tenant leak via `account_id` | Bug in adapter routes wrong tenant | Postgres RLS on `tenant_id`; bus subject includes `account_id` (cross-check in consumer) | Catastrophic — RLS is non-negotiable |
| Channel rename (zalo_oa case) | Legacy rows have old `channel_type` | Registry alias map, not UPDATE-in-place | Free with alias |
| Edit storm | Channel re-emits old messages with new `received_at` | `(account_id, source_message_id)` unique catches — silent dedup | Free at constraint level |
| Forum-post explosion | One Discord forum produces 10k threads | `kind=FORUM_POST` rows + index on `parent_conversation_id` | Manageable; partition by `tenant_id` if needed |

---

## 10. Migration paths (extensibility plays)

- **Adding a new channel** (e.g. WhatsApp at P10): adapter package + entry in `proto/channels.yaml` + outbound sender pool config. Zero schema change. **This is the litmus test mio plan.md P9 names — the recommended model passes it.**
- **Adding a new kind** (e.g. `VOICE_ROOM` for Discord stage / Slack huddle audio metadata): one constant in proto + one accepted value in `kind` enum + adapter handling. Old consumers ignore unknown kinds (forward compat via proto3 default behavior).
- **Adding federation** (Matrix-style): introduce `account.federation_origin` attribute + `conversation.federation_event_id`. The polymorphic table absorbs federated rooms as just another `kind`.
- **Splitting tenants** (one tenant becomes two): UPDATE `tenant_id` on accounts + cascade. With UUID v7 nothing renumbers.
- **Cross-channel identity merge** (Slack user X = Telegram user Y): set `channel_users.user_id` to a single canonical UUID. Goclaw's `merged_id` pattern, only cleaner here because `channel_users` already has the FK.
- **Reverse migration cost** (lock-in): if mio later wants per-channel-type tables (option A), a single SELECT...INSERT per channel does the job. Going *the other way* (de-merging) is the painful direction — and that's the direction option A takes you. Recommendation has lower lock-in.

---

## 11. Operational notes

### Indexes day 1
```
conversations (account_id, external_id) UNIQUE  -- the idempotent address
conversations (tenant_id)
conversations (account_id, kind, is_archived)
conversations (parent_conversation_id) WHERE parent_conversation_id IS NOT NULL
messages (account_id, source_message_id) UNIQUE -- idempotency
messages (tenant_id, conversation_id, received_at DESC)
messages (thread_root_message_id) WHERE thread_root_message_id IS NOT NULL
```

### RLS day 1
```sql
ALTER TABLE conversations ENABLE ROW LEVEL SECURITY;
CREATE POLICY tenant_isolation ON conversations
  USING (tenant_id = current_setting('mio.tenant_id')::uuid);
-- (and same for messages, accounts, channel_users)
```
RLS feels overkill for a POC — but goclaw's migration 27 audit found three places where the gateway forgot to filter by tenant. RLS is the backstop.

### `kind` taxonomy locked
The 7-value enum above maps every observed channel surface across all 7 platforms surveyed. If a future channel demands an 8th, it should be added by amendment to this doc, not by ad-hoc proto change.

### Pitfalls
- **Don't put `channel_instance_name`** on the message envelope (goclaw mistake). Use `account_id` + display name from join.
- **Don't compute `session_key` at gateway** and ship it — let consumers derive from `(tenant_id, account_id, conversation_id)`. Goclaw's deprecated comment is the canary.
- **Don't store oauth tokens in `account.attributes`** — vault them; reference by a vault key in attributes.

---

## 12. References

- Slack Conversations API and types — https://docs.slack.dev/apis/web-api/using-the-conversations-api/, https://api.slack.com/types/conversations
- Slack `chat.postMessage` (`thread_ts` semantics) — https://api.slack.com/methods/chat.postMessage
- Discord channel types — https://discord-api-types.dev/api/discord-api-types-v10/enum/ChannelType, https://docs.discord.com/developers/resources/channel
- Discord threads & metadata — https://docs.discord.food/topics/threads
- Mattermost API/channels schema — https://github.com/mattermost/mattermost-api-reference/blob/master/v4/source/channels.yaml, https://docs.mattermost.com/collaborate/channel-types.html
- Mattermost DB design write-up — https://snippets.aktagon.com/snippets/794-mattermost-database-schema-and-design
- Rocket.Chat room schema — https://developer.rocket.chat/reference/api/schema-definition/room
- Rocket.Chat threads — https://docs.rocket.chat/docs/threads
- Matrix m.thread (MSC3440) — https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/3440-threading-via-relations.md
- Matrix spec releases — https://github.com/matrix-org/matrix-spec/releases
- Zulip stream/topic model — https://zulipchat.com/help/about-streams-and-topics, https://zulip.com/why-zulip/
- Mautrix bridges (portal/ghost/double-puppeting) — https://docs.mau.fi/bridges/general/double-puppeting.html, https://matrix.org/docs/older/types-of-bridging/
- Sendbird Group/Open channels — https://sendbird.com/docs/chat/platform-api/v3/channel/channel-overview
- CloudEvents protobuf format — https://github.com/cloudevents/spec/blob/main/cloudevents/formats/protobuf-format.md
- Multi-tenant SaaS schema patterns — https://workos.com/blog/developers-guide-saas-multi-tenant-architecture, https://learn.microsoft.com/en-us/azure/architecture/guide/multitenant/approaches/messaging
- Canonical Data Model (Hohpe) — https://www.enterpriseintegrationpatterns.com/patterns/messaging/CanonicalDataModel.html
- Internal: goclaw migrations 1, 12, 14, 27, 35, 58 (`/Users/vanducng/git/personal/nextlevelbuilder/goclaw/migrations/`)
- Internal: goclaw `internal/channels/channel.go`, `internal/bus/types.go`

---

## 13. Open questions (need decisions before P1 commits)

1. **Edits = new message rows vs in-place UPDATE?** Recommendation suggests new rows with `MessageRelation.EDIT_OF`. Mio's current architecture doc §4 says "edit references the same `channel_message_id`" — needs reconciliation. Append-only is auditing-friendly and matches NATS replay semantics, but requires a "latest version" view. _Decision needed before SDK ships._
2. **`attributes` JSONB schema governance.** Proto has stable v1 fields; channel-specific data lands in JSONB. Need a written rule for when something graduates from JSONB to a proto field (suggest: ≥2 consumers OR ≥2 channels OR routing logic depends on it).
3. **Conversation member roster — table or attribute?** Slack/Mattermost have explicit `channel_members`. Goclaw skipped it (read members from cache). For mio: unclear if MIU agents need member listings. If yes → 5th table `conversation_members`. If no → punt.
4. **Compaction artifacts** (the `is_summary` field carried over from goclaw migration 12) — does the AI service write summaries back as messages (clean) or to a separate `summary` store? The proto includes `is_summary`; the storage policy isn't decided.
5. **`channel_type` registry location** — `proto/channels.yaml` vs `docs/channels.md` vs database table. YAML is recommended for CI gating; database is recommended for tenant-overridable behavior. They could coexist (YAML = known set, DB = enabled set per tenant).
6. **Federation in scope, ever?** Architecture doc §11 says "no" today. If "yes in 24 months", option C deserves a second look. If "no, ever", lock that in writing so we stop optimizing for it.
7. **Identity merge UX** — channel_users.user_id is nullable until merge happens. Who triggers merge? Heuristic (same email, same handle), explicit admin action, or both? P5+ concern but the schema accommodates either.

---

_End of report._
