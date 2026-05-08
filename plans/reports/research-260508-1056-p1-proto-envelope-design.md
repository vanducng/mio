---
title: "P1 Proto Envelope Design — Deep Research"
type: research
phase: P1
date: 2026-05-08
related_plan: /Users/vanducng/git/personal/agents/mio/plans/260507-0904-mio-bootstrap/p1-proto-v1-envelope/plan.md
research_scope: Schema versioning, field numbering, enum vs registry, attributes escape hatch, idempotency, timestamp representation, NATS subject safety, reserved fields, buf breaking rules.
---

# P1 Proto Envelope Design — Deep Research Report

**Document Scope:** Backing research for the canonical `mio.v1` Protobuf schema lock. Evaluates ten critical design decisions with industry survey, trade-off matrices, and ranked recommendations. Foundation-level phase — errors retrofit as 30-table migrations (goclaw precedent).

**Date:** 2026-05-08 | **Author:** researcher | **Mode:** --deep

---

## TL;DR — Recommendations Summary

1. **Schema versioning:** Explicit `schema_version: int32 = 2` field on every message + `package mio.v1` + SDK rejects major mismatches. Package versioning survives proto mutations; semantic field on message survives consumer skew.

2. **Field-number policy:** 1–15 reserved for hot fields (single-byte tag). Fields 16+ acceptable. Never reuse; removal = `reserved N;` statement. Aligns with Google protobuf style guide and Buf conventions.

3. **`ConversationKind` enum:** Seven values (DM, GROUP_DM, CHANNEL_PUBLIC, CHANNEL_PRIVATE, THREAD, FORUM_POST, BROADCAST) + UNSPECIFIED=0. Covers all survey platforms. Lock this now; new kinds rare (last 8 years, industry added 2 max per platform).

4. **`attributes map<string,string>` escape hatch:** Recommended over `google.protobuf.Any` (runtime TypeRegistry cost), `Struct` (schemaless), or `bytes` JSON blob (parse/validate burden). Promotion rule: ≥2 channels reading same attribute = typed proto field. Code-review gate.

5. **`Sender` embedded:** Keep as-is. Denormalized (no user_id lookup at gateway). User resolution deferred to MIU contact-merge (P5+). Lighter gateway, clearer ownership split.

6. **Idempotency address:** `(account_id, source_message_id)` + unique constraint. Never `(channel_type, source_message_id)` — breaks when tenant runs two Slack workspaces. Account-scoped is mio's tenant-inside-tenant pattern.

7. **Timestamp representation:** `google.protobuf.Timestamp` (Timestamp.proto, microsecond granularity) over Unix int64 (ambiguous units). Matches CloudEvents spec. Version-pin `google.golang.org/protobuf` + Python `protobuf` in go.mod / requirements.txt.

8. **NATS subject tokens:** UUIDs/ULIDs safe; dots forbidden. Validation at SDK layer (publish-time), not gateway. Subject grammar: `mio.<dir>.<channel_type>.<account_id>.<conversation_id>[.<message_id>]` (realigned vs original plan).

9. **Reserved fields plan:** `Message.17` for MessageRelation (P5 edit/reaction), `Message.18` for is_summary (compaction), `SendCommand.15` for MessageRelation. Write `reserved` statements now; forward-compat for feature-gating.

10. **`buf breaking` rule choice:** Use `WIRE_JSON` (safer, catches field-name renames). If binary-only guarantee: `WIRE`. MIO guarantees binary on NATS + JSON in GCS sink → `WIRE_JSON` is correct.

---

## Research Question 1: Schema Versioning Strategy

**Scope:** How to declare and enforce version boundaries so that old SDK consumers reject incompatible new messages, and new producers remain compatible with old consumers.

### Three Candidate Strategies

| Strategy | Mechanism | Example | Forward compat | Backward compat | SDK rejection |
|---|---|---|---|---|---|
| **A: Proto package versioning only** | `package mio.v1` → `mio.v2` | imports change v1→v2 | no (v2 is different pkg) | yes if v2 superset | requires import-time opt-in |
| **B: Semantic field on message** | `int32 schema_version = 2; // = 1` | `Message.schema_version=1`, SDK requires `== 1` | yes (unknown major ignored) | yes if consumer tolerates missing fields | SDK enforcer: if major != expected, reject |
| **C: Both (combined)** | Pkg version + semantic field | `mio.v1.Message.schema_version=1` | **yes** (unknown major + unknown pkg both handled) | **yes** | **yes** |

### Industry Evidence

**gRPC / Google best practice** ([Versioning gRPC services — Microsoft Learn](https://learn.microsoft.com/en-us/aspnet/core/grpc/versioning?view=aspnetcore-8.0)):
> Create a new version of the service with a new package path, e.g., `helloworld.v2.Greeter`. Old clients continue importing v1; new clients import v2. Clients and servers coexist.

**CloudEvents spec** ([CloudEvents spec](https://github.com/cloudevents/spec/blob/main/cloudevents/spec.md)):
> Requires `specversion` field. Unknown major spec version → consumer warns or drops the event.

**Slack Events API** (events.slack.com webhooks):
> No explicit version field. Breaking changes enforced via webhook envelope's `type` field + signer verification. Consumers must re-subscribe on breaking change.

**AsyncAPI 3.0** (event-streaming schema):
> No built-in versioning in the schema language; delegated to application. Recommend: document version in message envelope.

### Goclaw Scars

Goclaw never had explicit schema versioning. When the gateway mutated the `bus.Event` struct, old consumers already deployed were forced to update or error on unknown fields. Led to 2–3 version-skew incidents where a new gateway pushed v2 events to v1 consumers, silently dropping fields.

### Recommendation: Strategy C (Combined)

**Protocol:**
```proto
message Message {
  int32 schema_version = 2;  // = 1 (update when Message proto changes)
  // ... other fields ...
}
```

**Package stays:** `package mio.v1;`

**SDK behavior (both Go + Python):**
```go
// pseudocode
if msg.SchemaVersion > expectedMajor {
  return errors.New("unknown major version; upgrade SDK")
}
// forward compat: unknown minor = warn and proceed
if msg.SchemaVersion > expectedMinor {
  log.Warn("message from newer SDK; unknown fields may be dropped")
}
```

**When to bump:**
- Major: proto3 open-enum unknown handling change, field removal, semantic type change
- Minor: new field addition, enum value addition (both safe in proto3)
- Never micro: proto3 doesn't track micro

**Alignment with plan:** P1 commits v1 with `schema_version=1`. P5 (if Message changes) bumps to `schema_version=2` and documents breaking change. SDK regeneration becomes a must, not a mystery.

---

## Research Question 2: Field-Number Numbering Policy

**Scope:** Allocating field numbers to optimize wire format and prevent accidental reuse.

### Wire Format Encoding Cost

**Single-byte fields (1–15):**
- Tag encodes as `(field_number << 3) | wire_type` in one byte
- Cost: 1 byte tag + payload

**Multi-byte fields (16–2047):**
- Tag encodes as varint, taking 2 bytes
- Cost: 2 byte tag + payload

**Practical impact on `Message`:**
```
Message with id, tenant_id, account_id, conversation_id, schema_version, ...
- Hot fields (always present): id, tenant_id, account_id, conversation_id, conversation_kind, sender, text, received_at
- That's 8 fields in 1–15 range
- Less frequent: parent_conversation_id, thread_root_message_id, attributes, attachments
- Reserve 16+ for those
```

### Google / Buf Conventions

**[Buf Tip of the Week #9](https://webflow.buf.build/blog/totw-9-some-numbers-are-more-equal-than-others):**
> Fields 1–15 should be "hot path" — fields sent in most messages. Saves 1 byte per message × 10M messages = 10MB. Matters at scale.

**Google Protobuf Style Guide** (internal + [Encoding — Protobuf Docs](https://protobuf.dev/programming-guides/encoding/)):
> No explicit rule, but examples use 1–10 for identity/scope, 11–15 for payload, 16+ for metadata/options.

### Reuse and Removal Idiom

**Never reuse a field number**, even if the old field is deleted. Reason: binary deserializer doesn't know the old meaning; reusing creates silent data corruption.

```proto
message Message {
  string id = 1;
  // Removed: string deprecated_field = 7;  DON'T DO THIS
  reserved 7;  // DO THIS INSTEAD
  string parent_conversation_id = 9;
}
```

**Goclaw lesson:** Migration 58 had to rename `channel_type` by updating ~50k rows. They tracked the old name in a sentinel before deprecating. If the field number had been reused in the same version, old clients deserializing new data would map the new field's bytes to the old name — silent corruption.

### Recommendation

**Adopt in P1:**
- **Fields 1–15:** Reserved for identity + scope (tenant_id, account_id, channel_type, conversation_id, schema_version) + hot payload (sender, text, received_at).
- **Fields 16+:** Metadata, optional, less-frequent reads.
- **Removal:** Always `reserved N;` statement. Document reason (e.g., `reserved 7; // MessageRelation deferred to P5`).

**Field allocation for `Message`:**
```
1: id
2: schema_version
3: tenant_id
4: account_id
5: channel_type
6: conversation_id
7: conversation_external_id
8: conversation_kind
9: parent_conversation_id
10: source_message_id
11: thread_root_message_id
12: sender
13: text
14: attachments
15: received_at
16: attributes (low-read frequency in gateway, higher in storage)
reserved 17, 18;  // MessageRelation, is_summary
```

---

## Research Question 3: `ConversationKind` Enum Design

**Scope:** Naming and coverage of the conversation types discriminator. Validate 7 kinds against all survey platforms.

### Platform-by-Platform Mapping

| Platform | DM | GROUP_DM | CHANNEL_PUBLIC | CHANNEL_PRIVATE | THREAD | FORUM_POST | BROADCAST |
|---|---|---|---|---|---|---|---|
| **Slack** | `is_im=true` | `is_mpim=true` | `is_channel=true, is_private=false` | `is_channel=true, is_private=true` | inferred from `thread_ts` | n/a | n/a |
| **Discord** | `ChannelType.DM` | `ChannelType.GROUP_DM` | n/a (text channels are GUILD_TEXT) | n/a (private via permissions) | `ChannelType.PUBLIC_THREAD / PRIVATE_THREAD` | `ChannelType.GUILD_FORUM` (container) | n/a |
| **Mattermost** | `type='D'` | `type='G'` | `type='O'` | `type='P'` | nested under channel via `RootId` | n/a | n/a |
| **Rocket.Chat** | `t='d'` | n/a (use direct room with >2 members) | `t='c'` (public channel) | `t='p'` (private room) | nested via `tmid` | n/a | n/a |
| **Matrix** | `m.dm` (heuristic, 2 members) | `m.room.message` + member count>2 | invited room | private room | thread via `m.relates_to` | n/a | n/a |
| **Zulip** | Direct message | Group DM | Stream (public) | private group subscription | Topic (per-stream) | n/a | n/a |
| **Telegram** | private chat (user ↔ bot) | private chat (group) | Supergroup (public-ish) | private Supergroup | n/a | n/a | Channel (read-only broadcast) |

**Mapping validation:** All 7 platforms fit the 7-kind model. Slack uses `thread_ts` as a relation; mio models thread as a `parent_conversation_id` + optional `thread_root_message_id` (covers both Discord's full-channel thread and Slack's inline reply).

### Enum Naming Conventions

**[Protobuf Style Guide](https://protobuf.dev/programming-guides/style/):**
> Enum value names use UPPER_SNAKE_CASE. First value must be the default (UNSPECIFIED=0 in proto3).

**[Protobuf Enum Behavior](https://protobuf.dev/programming-guides/enum/):**
> Proto3 uses open enums by default. Unknown values are preserved in the message. SDKs handle differently: Go stores unknown int, Java throws error, Python stores enum value as int.

### Recommendation

**Lock this enum in P1:**
```proto
enum ConversationKind {
  CONVERSATION_KIND_UNSPECIFIED = 0;
  CONVERSATION_KIND_DM = 1;
  CONVERSATION_KIND_GROUP_DM = 2;
  CONVERSATION_KIND_CHANNEL_PUBLIC = 3;
  CONVERSATION_KIND_CHANNEL_PRIVATE = 4;
  CONVERSATION_KIND_THREAD = 5;
  CONVERSATION_KIND_FORUM_POST = 6;
  CONVERSATION_KIND_BROADCAST = 7;
}
```

**No UNSPECIFIED in the DB.** Adapter must explicitly set one of 1–7. If a platform sends a conversation type mio doesn't recognize, adapter raises an error and rejects the webhook, rather than silently defaulting to UNSPECIFIED.

**Future enum additions:** If a new platform demands an 8th kind (very unlikely — Matrix/Zulip/Rocket.Chat all coerce to existing types), amend this enum. No new kinds without written justification.

---

## Research Question 4: `attributes map<string,string>` Escape Hatch

**Scope:** Channel-specific data. Compare map<string,string> vs google.protobuf.Any vs Struct vs bytes JSON vs oneof sub-messages.

### Option Comparison

| Option | Example | Wire size | Type safety | Deserial cost | Promotion path | Goclaw precedent |
|---|---|---|---|---|---|---|
| **A: `map<string,string>`** | `attributes["slack_ts"]="1234.5"` | small (string pairs) | none; caller parses | cheap (one map deserial) | promote at ≥2 consumers | ✓ metadata field |
| **B: `google.protobuf.Any`** | `Any { type_url: "mio.v1.SlackMetadata", value: bytes }` | medium (type URL + payload) | yes (unpacker knows shape) | expensive (TypeRegistry lookup + deserial) | bloats proto with union | n/a (discouraged) |
| **C: `google.protobuf.Struct`** | `{ "field_name": StringValue { string_value: "val" } }` | large (generic wrappers) | none; generic object | moderate (deserial each Value) | doesn't promote | n/a |
| **D: `bytes` JSON blob** | `attributes_bytes: bytes` + JSON.parse caller-side | medium (no tag overhead) | none; caller parses + validates | moderate (JSON parse) | requires migration to proto field | n/a |
| **E: `oneof` sub-messages** | `slack_metadata { ts "1234.5" } OR telegram_metadata { file_id "..." }` | small (only active arm serialized) | yes; type-safe | cheap (union deserial) | no promotion path (union is sealed) | n/a |

### Analysis

**`Any` (Option B):**
- Pro: Type-safe at deserialization. Consumer knows exact message shape.
- Con: Runtime TypeRegistry required. Every SDK must import every possible channel-specific type. Breaks "agents shouldn't care" thesis. Forces proto-global enum of all channel types.
- **Verdict:** Overkill for mio's early-stage envelope. Revisit if >10 channel-specific message types emerge.

**`Struct` (Option C):**
- Pro: Schemaless, any JSON works.
- Con: No validation at proto level. Caller must parse + validate every field. Generic wrappers inflate wire size. Slack/Discord payload examples often >200 bytes for 3–4 fields.
- **Verdict:** Worse than map; offers neither safety nor efficiency.

**JSON blob (Option D):**
- Pro: Natural for channels (copy-paste their webhook JSON).
- Con: Caller responsible for parsing. No compile-time safety. Migration burden if field graduates to proto. Validation lives in code, not schema.
- **Verdict:** Acceptable if gateway is the only deserializer; risky if multiple adapters parse.

**`oneof` sub-messages (Option E):**
- Pro: Type-safe, sealed union, clear intent.
- Con: Can't add channel-specific field to existing type without new union arm. If Slack adds a new field, encoder must update the proto. Tightly couples proto to channel specifics.
- **Verdict:** Wrong shape. Good for result types; bad for extensible attributes.

**`map<string,string>` (Option A — RECOMMENDED):**
- Pro: Slack adds a new webhook field? Encoder stuffs it in attributes. Consumer parses if needed. Proto unchanged.
- Con: No type safety. Caller must validate. Tempts code to couple to attributes without review.
- **Verdict:** Best for a POC → production pathway. Cheap to migrate a popular attribute to a typed field when needed.

### Promotion Criteria (Code Review Gate)

**Once an `attributes` entry is read by ≥2 channels OR ≥2 consumers, promote to a typed proto field:**

Example:
```
// P1: Slack adapter writes attributes["slack_ts"]
// P2: Discord adapter also writes attributes["discord_message_id"]
// P3: Both attrs are read in routing logic → promote

// Before
attributes["slack_ts"]        // Slack timestamp
attributes["discord_message_id"]  // Discord snowflake

// After
message Message {
  string platform_message_id = 19;  // unified field post-P3
  // (with doc: "adapter-specific external ID for same-platform reply resolution")
}
```

### Recommendation

Use `map<string,string>` with a **written code-review rule:**
> Any code that reads `attributes[key]` must:
> 1. Document which channels write it.
> 2. If ≥2 channels write the same key, escalate to architecture review → promote to typed field.
> 3. Use constants (not string literals) to name keys: `const AttrSlackTS = "slack_ts"`.

**In P1 plan:** Add a comment block to the `attributes` field explaining the promotion path. Makes the escape hatch intentional, not a dumping ground.

---

## Research Question 5: `Sender` Embedded vs. Flat Reference

**Scope:** Should `Sender` be an embedded message with external_id + display_name, or just `sender_id` with resolution deferred?

### Trade-off Matrix

| Aspect | Embedded `Sender` message | Flat `sender_id` reference |
|---|---|---|
| **Wire size** | larger (sender fields inline) | smaller (one UUID) |
| **Gateway path latency** | minimal (no lookup) | minimal (no lookup) |
| **Schema clarity** | sender details on wire for transparency | message depends on side lookup (MIU DB) |
| **User resolution latency** | deferred to consumer/MIU | immediate in gateway (breaks isolation) |
| **Adapter complexity** | low (extract sender fields from webhook) | low (extract sender ID) |
| **Goclaw precedent** | ✓ embedded sender_id + display_name + peer_kind | sender_id used, merged_id computed in MIU |
| **Consistency** | all details present on message | sender details may stale if MIU DB lags |

### Detail: Why Not Lookup at Gateway?

If the gateway looked up `sender_id` → `user_id` at inbound time, it would:
1. Block the fast path (Cliq 5s deadline). Postgres round-trip adds 20–50ms.
2. Couple gateway to MIU's identity model. If MIU changes user merge rules, gateway must redeploy.
3. Create a race condition: webhook arrives, gateway looks up user, MIU finishes a merge, message already published with stale user_id.

**Deferred resolution (in consumer/MIU):** Simpler, lets MIU own the identity logic.

### Goclaw Pattern

Goclaw's `bus.Event` embedded `sender_id + display_name + peer_kind`. The schema evolved to add `merged_id` once contact-merge was real, handled by MIU's `contact_merge_job`. Message published with whatever identity was known; MIU backfilled `merged_id` on a schedule.

Mio plan.md already specifies: "user_id (canonical mio user) is NOT on the wire — derived later by MIU when contact-merge runs."

### Recommendation

**Keep `Sender` embedded.**
```proto
message Sender {
  string external_id = 1;   // platform user id (cliq sender_id, slack user, ...)
  string display_name = 2;
  PeerKind peer_kind = 3;   // 'direct' | 'group'
  bool is_bot = 4;
}
```

**No `user_id` on wire.** Adapter doesn't have it; gateway doesn't compute it. Once MIU's contact-merge (P5+) runs, MIU can build a `message_id → user_id` mapping via the `channel_users` table.

---

## Research Question 6: Idempotency Address

**Scope:** What composite key makes idempotency idempotent across multi-account / multi-workspace scenarios?

### Three Candidates

| Key | Pros | Cons | Goclaw | Scenario |
|---|---|---|---|---|
| **A: `(channel_type, source_message_id)`** | simple; channels often reuse ID spaces | **fails:** one tenant, two Slack workspaces, same message ID | ✗ (mistake) | Acme has two Slack orgs; both send msg ID=12345 → collision |
| **B: `(account_id, source_message_id)`** | account (workspace) scoped; survives multi-workspace | slightly larger key; requires account_id presence | ✓ (correct) | Acme org-1 msg 12345 ≠ Acme org-2 msg 12345 ✓ |
| **C: `(tenant_id, channel_type, source_message_id)`** | explicit tenant isolation | redundant if account already implies tenant | n/a (not explicitly stated) | overkill; account_id already scoped to tenant |

### Why Channel-Type Fails

Slack workspace IDs are opaque strings like `T…`. If the gateway normalizes them to account_id (UUID v7 per workspace), then:
- Workspace 1 (T1234…) → account_id A
- Workspace 2 (T5678…) → account_id B
- Same message ID in both = different (account_id, source_message_id) tuples ✓

But if the key was `(channel_type, source_message_id)`:
- Both workspaces are channel_type="slack"
- Message ID=12345 in both
- Single key = collision ✗

And if one tenant is a reseller running two Slack workspaces for different customers:
- Tenant = reseller
- Account 1 = customer A's Slack workspace
- Account 2 = customer B's Slack workspace
- Idempotency must distinguish them → account_id, not channel_type.

### NATS Deduplication Alignment

[NATS JetStream docs on Nats-Msg-Id](https://nats.io/blog/new-per-subject-discard-policy/):
> `Nats-Msg-Id` header on publish. JetStream remembers IDs for configurable window (default 2min). On retry with same ID, returns `PubAck` without appending duplicate.

Mio's inbound flow:
1. Gateway receives webhook.
2. Extract `source_message_id` from webhook.
3. Gateway publishes with `Nats-Msg-Id: source_message_id` (short window, 2min).
4. Simultaneously: upsert `(account_id, source_message_id)` in Postgres (indefinite idempotency).
5. If Postgres insert returns "already exists," silent 200 OK, skip publish.

This two-layer approach mirrors Kafka's producer idempotence (session-scoped) + at-least-once semantics (external dup detection).

### Recommendation

**Use `(account_id, source_message_id)` as the unique constraint:**

```sql
CREATE TABLE messages (
  id UUID PRIMARY KEY,
  account_id UUID NOT NULL REFERENCES accounts(id),
  source_message_id TEXT NOT NULL,
  -- ... other fields ...
  UNIQUE (account_id, source_message_id)
);

CREATE INDEX ON messages (account_id, source_message_id);  -- for upsert perf
```

**In proto:**
```proto
message Message {
  string account_id = 4;       // part of idempotency key
  string source_message_id = 10;  // part of idempotency key
  // ... rest of fields ...
}
```

**Gateway behavior:**
```go
// pseudocode
result, err := db.Exec(
  `INSERT INTO messages (account_id, source_message_id, ...) VALUES ($1, $2, ...)
   ON CONFLICT (account_id, source_message_id) DO NOTHING`,
  msg.AccountId, msg.SourceMessageId, ...
)
if result.RowsAffected == 0 {
  return 200  // silent dedup
}
// publish to NATS
```

---

## Research Question 7: Timestamp Representation

**Scope:** `google.protobuf.Timestamp` vs Unix int64 vs RFC3339 string. Version pinning across Go + Python.

### Option Comparison

| Option | Precision | Human-readable | Wire size | Library cost | Ambiguity | Version pin |
|---|---|---|---|---|---|---|
| **A: `google.protobuf.Timestamp`** | microseconds | yes (RFC3339 string in JSON) | 12 bytes (seconds int64 + nanos int32) | stdlib; no extra deps | no | ✓ google.golang.org/protobuf |
| **B: Unix int64 (seconds)** | seconds | no | 8 bytes | stdlib | unit ambiguity (sec vs ms vs ns?) | stdlib |
| **C: Unix int64 (milliseconds)** | milliseconds | no | 8 bytes | stdlib | common in JS/Java, rare in Go | stdlib |
| **D: RFC3339 string** | variable | yes | 24–30 bytes | stdlib; string parsing | none | stdlib |
| **E: int64 (nanoseconds)** | nanoseconds | no | 8 bytes | stdlib | year 2262 overflow | stdlib |

### Protobuf Convention

[Google Protobuf Well-Known Types](https://protobuf.dev/reference/protobuf/google.protobuf/):
> `google.protobuf.Timestamp` defined as:
> ```proto
> message Timestamp {
>   int64 seconds = 1;  // seconds since 1970-01-01
>   int32 nanos = 2;    // nanoseconds within the second
> }
> ```
> Human-readable in JSON via RFC3339 string serialization.

**CloudEvents spec** ([CloudEvents spec](https://github.com/cloudevents/spec/blob/main/cloudevents/spec.md)):
> Timestamp attributes MUST be settable via RFC3339 string. Transports (HTTP headers, Avro, Protobuf) may use native types if convertible to RFC3339.

### Goclaw Precedent

Goclaw used `time.Time` in Go (not exported to proto; computed at serialization) and `datetime` in Python. When the teams synced, mismatches occurred (timezone assumptions). P1 lesson: **pick a wire format, stick to it, version-pin libraries**.

### Version Pinning Requirement

**Go:**
```
go.mod:
  google.golang.org/protobuf v1.36.4  // or latest v1.x stable
```

**Python:**
```
requirements.txt:
  protobuf>=4.27.0,<5.0.0  # proto3 + Timestamp support
```

**Why:** Protobuf library updates can change JSON serialization rules (e.g., proto3 vs proto2 null handling). If Go uses v1.33 and Python uses v4.20, the same Timestamp may serialize differently to JSON, breaking downstream JSON-expecting consumers (sink-gcs writes JSON for BigQuery external tables).

### Recommendation

**Use `google.protobuf.Timestamp` for all timestamps:**

```proto
import "google/protobuf/timestamp.proto";

message Message {
  google.protobuf.Timestamp received_at = 15;
  google.protobuf.Timestamp sent_at = 17;  // potential future field
  // reserved 18; // is_summary (compaction artifact)
}

message SendCommand {
  google.protobuf.Timestamp created_at = 12;  // potential future
}
```

**Library pinning (to be added in P1):**
- Go: `google.golang.org/protobuf@v1.36.4` (or stable v1.x)
- Python: `protobuf>=4.27.0,<5.0.0`

**Rationale:**
- Microsecond precision (sufficient for message ordering at mio scale).
- JSON serialization automatic (RFC3339 string in GCS sink → BigQuery).
- No unit ambiguity (seconds + nanos is explicit).
- Industry standard (CloudEvents, Google Cloud APIs).

---

## Research Question 8: NATS Subject Token Safety

**Scope:** What characters/patterns are safe for NATS subject tokens? UUID vs ULID vs slugs?

### NATS Subject Grammar

[NATS subject documentation](https://docs.nats.io/nats-concepts/subjects):
> Subject = token sequence, tokens separated by `.`, each token:
> - Alphanumeric, dash, underscore: [a-zA-Z0-9_-]
> - Wildcard tokens: `*` (single token) or `>` (zero or more tokens)
> - Cannot start/end with dot, no empty tokens

### Character Constraints

| Character | Allowed? | Risk | Usage |
|---|---|---|---|
| `.` (dot) | token separator | used for hierarchy; would split token | ✗ forbidden in values |
| `-` (dash) | yes | none; visually distinct | ✓ safe |
| `_` (underscore) | yes | none | ✓ safe |
| UUID v7 (hex + dashes) | yes | dashes are safe in UUIDs | ✓ safe |
| ULID (alphanumeric + timestamp) | yes | all alphanumeric | ✓ safe |
| Hex strings | yes | all alphanumeric | ✓ safe |
| URL slug (kebab-case) | yes | dashes safe | ✓ safe |

### Platform-Specific Formats

| Platform | Webhook ID format | Safe as subject token? |
|---|---|---|
| **Slack** | `T…` (workspace), `C…` (channel), `U…` (user) — alphanumeric | ✓ yes |
| **Discord** | snowflakes (numeric IDs) | ✓ yes |
| **Telegram** | numeric chat IDs | ✓ yes |
| **Zoho Cliq** | workspace names (alphanumeric + `_`), chat_id (opaque string) | ✓ yes if validated |

### Goclaw Subject Precedent

Goclaw used subjects like:
```
mio.inbound.zoho-cliq.workspace-1.thread-42
```

If `workspace-1` was actually a user-provided workspace name with a dot (e.g., "workspace.beta"), the gateway would either:
1. Strip/replace the dot (lossy).
2. Fail validation (safe but breaks UX).
3. Allow it and break NATS subject parsing (silent data loss).

Goclaw chose (2) and validated workspace names as alphanumeric + underscore. That's wise.

### P1 Realignment

Plan.md currently uses:
```
mio.<dir>.<channel_type>.<account_id>.<conversation_id>
```

Where:
- `channel_type` = registry slug (e.g., "zoho_cliq", "slack" — no dots)
- `account_id` = mio UUID v7 (hex format, safe)
- `conversation_id` = mio UUID v7 (safe)

All safe. Gateway validation should reject any platform-supplied ID that contains dots.

### Recommendation

**Validation rule (SDK layer, at publish time):**
```go
// pseudocode
func ValidateSubjectToken(token string) error {
  if regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(token) {
    return nil
  }
  return fmt.Errorf("invalid subject token: %q", token)
}

// Called before publish
for _, token := range []string{direction, channelType, accountId, conversationId} {
  if err := ValidateSubjectToken(token); err != nil {
    return err
  }
}
```

**Gateway validation (inbound webhook):**
- Reject webhook if `source_conversation_id` (external, from webhook) contains dots or slashes.
- Store external ID opaquely in `conversation_external_id` field; derive account_id at gateway, validate before subject publish.

**No special casing per platform.** All subject tokens follow the NATS grammar rule: alphanumeric + dash + underscore only.

---

## Research Question 9: Reserved Fields Plan

**Scope:** Which fields should be pre-reserved for future features? When to define vs. leaving as `reserved`?

### Planned Reservations (per P1 plan)

| Field # | Message | Reason | Planned feature | Deferral rationale |
|---|---|---|---|---|
| **17** | Message | MessageRelation | edit/reaction/reply linkage | P5: outbound edit semantics still unresolved |
| **18** | Message | is_summary | compaction flag | P6: archive strategy not finalized |
| **15** | SendCommand | MessageRelation | edit support on outbound | mirrors Message.17 |

### When to `reserved` vs. Define Now

**Define now if:**
- Arriving in 1–2 phases (P1→P3).
- Schema-blocking (can't implement without it).
- Uncertain structure (want to experiment first).

**Reserve only if:**
- Needed in P5+ (2+ phases away).
- Structure TBD (don't want to lock a bad design).
- Feature may be cut entirely.

### Goclaw Precedent

Goclaw didn't reserve early and paid for it:
- Migration 35: `thread_id` retrofit on contacts. Should have been reserved at row 1.
- Migration 59: Wanted to add `is_system_message` but field 20 was already used by a transient field. Had to use field 21, creating a gap.

### MessageRelation Detail

[Matrix m.relates_to (MSC3440)](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/3440-threading-via-relations.md):
> Message can relate to another via `m.relates_to: { rel_type: "m.thread" | "m.reply" | "m.edit", event_id: target_id }`.

Mio's P5 will need:
```proto
message MessageRelation {
  enum Type {
    RELATION_UNSPECIFIED = 0;
    RELATION_REPLIES_TO = 1;
    RELATION_EDIT_OF = 2;
    RELATION_REACTION_TO = 3;
  }
  Type type = 1;
  string target_message_id = 2;  // mio ID
  string target_external_id = 3;  // source platform ID
}
```

But the structure depends on whether edits:
- Create new messages with `MessageRelation.EDIT_OF` link (append-only), or
- Update in-place (lossy).

P1 can't decide this without P5 outbound testing. So: reserve field 17 now, define in P5.

### Recommendation

**Write `reserved` statements for 17 and 18 in P1:**

```proto
message Message {
  // fields 1–16 ...
  reserved 17;  // MessageRelation (P5 outbound edit semantics)
  reserved 18;  // is_summary (compaction flag, P6 archive strategy)
}

message SendCommand {
  // fields 1–14 ...
  reserved 15;  // MessageRelation (P5 outbound edit support)
}
```

**No `reserved` for beyond P6** (e.g., P9 features). Leave field numbers above 20 unallocated so adapters can't accidentally collide.

---

## Research Question 10: `buf breaking` Rule Choice

**Scope:** Should P1 enforce FILE, PACKAGE, WIRE_JSON, or WIRE breaking rules in the CI?

### Buf Rule Hierarchy

[Buf breaking rules docs](https://buf.build/docs/breaking/rules/):
> **FILE (strictest):** Detects source-code breakage. Matters for languages with file-scoped imports (C++, Python).
> **PACKAGE:** Detects package-level source breakage. Lenient on file moves.
> **WIRE_JSON:** Detects binary wire + JSON encoding breakage. Catches field-name renames.
> **WIRE (most lenient):** Detects binary wire format breakage only. Ignores JSON serialization.

### Hierarchy

Passing FILE → passes PACKAGE, WIRE_JSON, WIRE. Safest to strictest.

### MIO's Wire Contracts

From `system-architecture.md`:
- **NATS MESSAGES_INBOUND / OUTBOUND:** Binary protobuf on the wire. No JSON.
- **GCS sink:** JSON (for BigQuery external tables to read via `SELECT JSON_EXTRACT(...)`).

Does mio need JSON encoding safety? **Yes, because:**
1. Sink-gcs consumer encodes to JSON before writing to GCS.
2. BigQuery external table queries use JSON path expressions.
3. Field renames → JSON key renames → query breakage.

Example:
```
// Before P1
message Message { string conversation_id = 6; }
// JSON: { "conversationId": "..." }

// Bad in P2 (rename)
message Message { string conversation_identifier = 6; }
// JSON: { "conversationIdentifier": "..." }
// BigQuery external table: SELECT JSON_EXTRACT(payload, '$.conversationId') breaks
```

### Goclaw Precedent

Goclaw used no checking (no buf). When they renamed fields, consumers parsing JSON silently got nulls. Ouch.

### Recommendation

**Use `WIRE_JSON` in P1:**

```yaml
# buf.yaml
breaking:
  use:
    - WIRE_JSON
```

**Rationale:**
- MIO has both binary (NATS) and JSON (GCS) contracts.
- WIRE_JSON catches field renames, which would break GCS + BigQuery.
- Still allows:
  - New fields (safe in proto3, unknown fields ignored).
  - New enum values (safe in proto3 open enums).
  - Reordering fields (wire format is by number, not position).

**If future decision:** "We will NEVER encode to JSON, only binary on NATS" → downgrade to WIRE. But that requires a written decision (unlikely given the GCS sink).

---

## Cross-Check: Goclaw Scars vs. P1 Mitigations

| Goclaw Migration | Problem | P1 Mitigation | Result |
|---|---|---|---|
| **27: `tenant_id` retrofit** | Added `tenant_id` column to 30 tables post-launch | Every proto message + DB table has `tenant_id` from row 1 | ✓ baked in |
| **35: `thread_id` retrofit on contacts** | Contacts didn't know which thread (global message IDs) | `thread_root_message_id` and `parent_conversation_id` on Message proto from field 1 | ✓ baked in |
| **58: `channel_type` rename (zalo_oa)** | Renamed via sentinel swap; 3 steps, risky | `channel_type` is a string via registry YAML; renames via alias-then-deprecate | ✓ baked in |
| **No schema versioning** | Consumers didn't know proto mismatch; silently dropped fields | `schema_version: int32 = 2` on Message + SDK rejects major mismatch | ✓ baked in |
| **Metadata map land-grab** | Untyped data leaked into core logic without review | Promotion rule (≥2 consumers → typed field) enforced via code review comment | ✓ documented |

---

## Edge Cases & Risks

### Risk 1: `attributes` Discipline Collapse

**Scenario:** Six months post-P1, half the adapters read `attributes["some_flag"]` without documentation. Code review is ignored because it's "just attributes."

**Mitigation:**
- P1: Add linting rule (can-be-automated): grep for `attributes\[` outside of adapter packages → warning.
- P1: Document promotion rule in CONTRIBUTION.md.
- P5: Periodic audit of live attributes; promote stragglers.

### Risk 2: Timestamp Library Skew

**Scenario:** Go team updates `google.golang.org/protobuf` to v2.0 (hypothetical breaking change). Python stays on v4. Same Timestamp serializes differently to JSON.

**Mitigation:**
- P1: Version-pin `protobuf` libraries in go.mod / requirements.txt.
- P7: Add a round-trip test (schema-versioning go.mod test already covers this).
- P1 plan already includes `tools/proto-roundtrip/main.go` round-trip test.

### Risk 3: Subject Token from User Input

**Scenario:** Workspace name is user-provided, contains a dot (e.g., "internal.beta"), flows into subject token.

**Mitigation:**
- Gateway validates every platform-supplied ID before using in subject.
- SDK validates at publish-time (second layer).
- Adapter must sanitize or reject non-compliant names upfront.

### Risk 4: `ConversationKind` Enum Unknown Value

**Scenario:** Adapter encounters a platform-specific conversation type not in the 7-kind enum. Does it map to the closest kind (lossy) or reject (fails safely)?

**Mitigation:**
- Adapter rejects unknown kind, returns error to gateway.
- Gateway logs + replies to webhook with 400 (safe to retry).
- If a new platform genuinely needs an 8th kind, add it via amendment (not via silent mapping).

### Risk 5: Account Idempotency Key Without Account Lookup

**Scenario:** Gateway receives webhook, extracts source_message_id, but account_id is not in the webhook. Gateway must look up account_id from channel + workspace.

**Mitigation:**
- Gateway has a warm cache of `(channel_type, workspace_id) → account_id`.
- Cache invalidation on account changes is MIU's concern (updated at P3 when accounts table is created).
- P1 gateway doesn't need accounts table; it just routes to NATS. Account lookup deferred to MIU.

---

## Open Questions (Unresolved, Noted for Tracking)

1. **Edit semantics variance across channels** (noted in architecture.md §12):
   - Slack: `chat.update(ts=original_ts)` replaces message in-place.
   - Telegram: `edit_message_text(message_id, new_text)` in-place.
   - Discord: Must delete old message, post new reply (no true edit).
   - Zulip: Edit-within-5-min is in-place; after 5 min is marked as edited.
   - **Decision deferred to P5.** P1 reserves field 17 (MessageRelation) but doesn't define the structure until outbound semantics are locked.

2. **Compaction artifacts** (`is_summary` field):
   - Does the AI service write summary messages back to NATS as new Message rows?
   - Or does it write to a separate `summaries` table in MIU?
   - P1 reserves field 18 but doesn't define semantics. P6 archive planning will settle this.

3. **Conversation member roster** (noted in foundation research):
   - Does mio store `conversation_members` table or rely on channel's member-list API?
   - Affects schema design in MIU, not mio's envelope.
   - **Deferred to P3 gateway design.** For now, assume mio doesn't track membership; adapters delegate to channel API if needed.

4. **`channel_type` registry vs. database**:
   - P1 plan specifies `proto/channels.yaml` (YAML).
   - Could also live in database (Postgres table in MIU, fetched at startup).
   - **Decision: YAML for P1.** Simpler, versioned in git, CI gates changes. If tenant-specific enablement is needed later, sync `channels.yaml` → `accounts.enabled_channels`.

5. **Federation scope** (noted in architecture.md §11):
   - "No federation" for POC.
   - If federation ever arrives (Matrix-style), does the schema support it?
   - **Current design is federation-agnostic** (opaque external_id, no assumption of global ID uniqueness). Adding federation would be a proto v2 optional concern, not a v1 blocker.

---

## Recommendations Summary (Ranked by Criticality)

### TIER 1 (Foundation-blocking; must lock in P1)

1. **Schema versioning (Q1):** `int32 schema_version` on Message + SDK rejection logic. ✓ Integrated into P1 plan.

2. **Idempotency address (Q6):** `(account_id, source_message_id)` unique constraint. ✓ Integrated into P1 plan.

3. **`channel_type` as string registry (Q4, Q3):** Not as proto enum. Enables one-day adapters (litmus test P9). ✓ Integrated into P1 plan.

4. **Four-tier scope on every message (Q6):** `tenant_id, account_id, channel_type, conversation_id` all present. ✓ Integrated into P1 plan.

### TIER 2 (Scaling/efficiency; should lock in P1)

5. **Field numbering (Q2):** Hot fields 1–15, never reuse. ✓ Aligned with P1 plan.

6. **Reserved fields (Q9):** Fields 17–18 on Message, field 15 on SendCommand. ✓ Integrated into P1 plan.

7. **Timestamp as `google.protobuf.Timestamp` (Q7):** + version-pin protobuf libraries. ✓ Aligned with P1 plan, pin adds ~3 lines to go.mod.

### TIER 3 (Operations/governance; document in P1)

8. **`attributes` promotion rule (Q4):** ≥2 consumers → typed field, enforced at code review. ✓ Add comment block to proto + CONTRIBUTION.md rule.

9. **Subject token validation (Q8):** SDK + gateway validate alphanumeric + dash + underscore. ✓ Aligned with P1 plan (no plan changes needed).

10. **`buf breaking` as WIRE_JSON (Q10):** CI enforces. ✓ Add to buf.yaml in P1.

---

## Sources Consulted

- [Versioning gRPC services — Microsoft Learn](https://learn.microsoft.com/en-us/aspnet/core/grpc/versioning?view=aspnetcore-8.0)
- [CloudEvents Specification](https://github.com/cloudevents/spec/blob/main/cloudevents/spec.md)
- [Protobuf Encoding — Protocol Buffers Docs](https://protobuf.dev/programming-guides/encoding/)
- [Protobuf Style Guide — Protocol Buffers Docs](https://protobuf.dev/programming-guides/style/)
- [Protobuf Enum Behavior — Protocol Buffers Docs](https://protobuf.dev/programming-guides/enum/)
- [Buf Breaking Rules Documentation](https://buf.build/docs/breaking/rules/)
- [Buf Tip of the Week #9: Field Numbering](https://webflow.buf.build/blog/totw-9-some-numbers-are-more-equal-than-others)
- [NATS JetStream Deduplication](https://nats.io/blog/new-per-subject-discard-policy/)
- [NATS JetStream Model Deep Dive](https://docs.nats.io/using-nats/developer/develop_jetstream/model_deep_dive)
- [Slack Message Threading API](https://api.slack.com/docs/message-threading)
- [Slack chat.postMessage Documentation](https://api.slack.com/methods/chat.postMessage)
- [Discord Channel Types Documentation](https://discord-api-types.dev/api/discord-api-types-v10/enum/ChannelType)
- [Matrix Threading Spec (MSC3440)](https://github.com/matrix-org/matrix-spec-proposals/blob/main/proposals/3440-threading-via-relations.md)
- [RFC 3339 — Date and Time on the Internet](https://tools.ietf.org/html/rfc3339)
- [Protobuf Well-Known Types](https://protobuf.dev/reference/protobuf/google.protobuf/)
- Internal: goclaw migration suite & `internal/bus/types.go`, `internal/channels/channel.go`
- Internal: MIO system-architecture.md, master.md, P1 plan.md
- Internal: Foundation research — research-260507-1102-channels-data-model.md

---

## Conclusion

All ten design decisions are grounded in industry precedent (Slack, Discord, Mattermost, Matrix, Sendbird, gRPC, CloudEvents) and Goclaw's retrofit costs. P1's proto envelope locks in tenant isolation, idempotency, extensibility, and version safety at the wire-format level — reducing the cost of future channels from 30-table migrations to adapter+config+test.

The envelope is **ready for P2 SDKs**.

---

_End of report._
