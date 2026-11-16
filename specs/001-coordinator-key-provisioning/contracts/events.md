# Contract: structured events and alerts

**Feature**: `001-coordinator-key-provisioning` | **Date**: 2026-11-12

The hub emits exactly one kind of telemetry: a JSON event on stdout through `log/slog`. There are
no metrics, no traces, no other log path, and no free-text logging. This document fixes the field
set, the catalogue, the redaction rules, and the three alerts derived from the stream.

---

## 1. Envelope

Every event is one JSON object on one line.

```json
{
  "ts": "2026-11-12T18:04:31.482Z",
  "level": "info",
  "schema": 1,
  "event": "driver.ready",
  "msg": "driver ready",
  "request_id": "3f1c0a8e-2b47-4f0e-9a1c-6d5e8b2f7a10",
  "coordinator_id": "usb:10c4:ea60:00a1b2c3",
  "home_id": "0xdeadbeef",
  "schema_version": 52,
  "start_ms": 3412
}
```

| Field | Presence | Type | Rule |
|---|---|---|---|
| `ts` | always | RFC 3339 UTC, milliseconds | Emitted by the handler, not by call sites, so it cannot drift. |
| `level` | always | `debug` \| `info` \| `warn` \| `error` | `info` is the production floor; debug is never enabled on a deployed hub. |
| `schema` | always | integer, currently `1` | Bumped only for a breaking field change (§6). |
| `event` | always | string, `domain.action` | Closed set from §3. A new value requires a change to this document. |
| `msg` | always | string | One short constant sentence. Never interpolates a value, a path, or an error. |
| `request_id` | when HTTP-caused | string | Same value as the response's `X-Request-Id` and the audit row's `request_id`. |
| `actor_addr` | when HTTP-caused | string | Client network address, `host:port` stripped to host. The audit trail's actor (FR-019). |
| `actor_label` | when supplied | string ≤ 64 | From `X-Actor-Label`. Self-declared, unverified, control characters stripped. |
| `coordinator_id` | when scoped | string | `usb:<vid>:<pid>:<serial>` or `usb:<vid>:<pid>:port:<topology>`. |
| `home_id` | when known | `0x` + 8 hex | Never a decimal integer, so it is never confused with a node ID. |

**Naming**: `snake_case` for fields, `domain.action` for event names, past tense for things that
happened (`driver.failed`) and present participle for things that are starting (`driver.starting`).

**Correlation**: `request_id` is the same value in the response header, the error's `request_id`,
the audit row and every event caused by that request. `coordinator_id` plus `ts` correlates driver
lifecycle across restarts.

---

## 2. Redaction

Fields are built from an allow-list. A value that is not on the list cannot reach the logger,
because the logger only accepts the typed event structs in `internal/event`, not
`map[string]any`.

**Allowed to appear**

- identifiers: `coordinator_id`, `request_id`, `home_id`, USB `vid`/`pid`/`serial`, `node_id`,
  `security_class` (the **name**, e.g. `S2_Authenticated` — never the value)
- verdicts and counts: `outcome`, `reason`, `code`, `node_count`, `secure_node_count`, `revision`,
  `applied_revision`, `attempt`, `consecutive_failures`, `duration_ms`, `start_ms`, `status`
- hardware descriptors: `manufacturer`, `product`, `firmware_version`, `sdk_version`, `chip_type`
- paths: `serial_path`, `data_dir`, but only as the `/dev/serial/by-id/...` or configured value
- configuration: `bind`, `log_level`, `destructive_ops` (`enabled`/`disabled` only)

**Never allowed to appear — in any field, at any level, in any event**

| Secret | Why |
|---|---|
| Key values, in any encoding | FR-013, SC-004. A single hex string in a log line defeats the whole export gate. |
| The master key, or any ciphertext, nonce or key blob | It is the thing that protects the keys. |
| `confirm_token` values | A live token is a 120-second capability to export keys or reset the network. Log `operation` and `request_id`, never the token or its hash. |
| Rendered config file content, or any fragment of it | It is nothing but key material and paths. |
| The DSK or any `dsk` field | Same class of secret as a key. |
| `Authorization`, `Cookie`, and all raw request headers | Not used by this API, and logging headers wholesale leaks whatever a proxy adds. |
| Panic values and stack traces | A stack can embed a key if one was in scope. `api.panic` records the **type** of the panic value only, plus the route. |
| Raw request and response bodies | The key submission body contains keys; error bodies can echo input. |

**Enforcement**: `internal/api/presenters.go` is the only constructor of response bodies, and
`internal/event` is the only constructor of log records. Both are covered by a test that scans the
rendered output of every handler and every event constructor for a 32-hex-character run and fails if
one appears — the same test that protects SC-004 (`go test ./internal/... -run TestNoKeyMaterial`).

---

## 3. Catalogue

Every field named in a row is always present on that event. Optional context comes from §1 only.
`level` is the fixed level; a variable level is stated explicitly.

### 3.1 Lifecycle

| Event | Level | Additional fields |
|---|---|---|
| `hub.started` | info | `version`, `go_version`, `db_schema`, `bind`, `data_dir`, `destructive_ops` |
| `hub.stopping` | info | `reason` (`signal` \| `fatal`), `uptime_s` |
| `hub.config_invalid` | error | `field`, `reason`. The process exits non-zero after this event; it is the only startup failure path. |
| `store.migrated` | info | `from_version`, `to_version`, `duration_ms` |
| `store.integrity_failed` | error | `detail` (a fixed code, not the SQLite message verbatim) |

### 3.2 HTTP boundary

| Event | Level | Additional fields |
|---|---|---|
| `api.request` | info | `method`, `route` (the pattern, e.g. `/api/v1/keys`, never the raw path with an ID), `status`, `duration_ms`, and `code` when `status >= 400` — the Problem's machine-readable code. This is the access log. One event per request; a failed request is not a second event, `status` and `code` are the signals. |
| `api.panic` | error | `route`, `panic_type`. Recovered; the request returns `500 internal_error`. No value, no stack. |

### 3.3 Discovery

| Event | Level | Additional fields |
|---|---|---|
| `coordinator.discovered` | info | `identity_kind`, `vid`, `pid`, `serial_path` |
| `coordinator.removed` | info | `last_path` |
| `coordinator.support_changed` | info | `from`, `to`, `reason` |
| `coordinator.selected` | info | `previous_coordinator_id`, `reason` (`operator` \| `restored`) |
| `coordinator.rejected` | warn | `code` (`coordinator_unsupported` \| `coordinator_network_mismatch` \| `ambiguous_identity`) |

### 3.4 Driver

| Event | Level | Additional fields |
|---|---|---|
| `driver.starting` | info | `attempt`, `serial_path`, `schema_version` (after negotiation, before `start_listening`) |
| `driver.ready` | info | `schema_version`, `start_ms`, `node_count`, `secure_node_count` |
| `driver.failed` | warn | `attempt`, `reason` (`spawn_failed` \| `handshake_timeout` \| `protocol_unsupported` \| `start_listening_failed` \| `exited` \| `watchdog` \| `config_render_failed`), `exit_code`, `signal`, `duration_ms`, `next_retry_at` |
| `driver.stopped` | info | `reason` (`requested` \| `faulted` \| `shutdown`) |
| `driver.restart_paused` | error | `consecutive_failures`, `since`. Retrying has stopped until the device set changes. |
| `driver.log` | info/warn | `line` — the child's own output, length-capped at 4 KiB, with the redaction pass applied (a security-details log line is dropped entirely, not masked). Level follows the child's level. |

### 3.5 Keys

| Event | Level | Additional fields |
|---|---|---|
| `keys.stored` | info | `revision`, `classes`, `source` (`supplied` \| `generated`) |
| `keys.applied` | info | `revision`, `applied_revision`, `duration_ms` |
| `keys.apply_failed` | warn | `revision`, `applied_revision`, `reason` |
| `keys.in_use_rejected` | info | `classes`, `secure_node_count` |
| `keys.corrupt` | error | `classes`, `detected_by` (`aead` \| `integrity_check`). **Alert trigger.** |
| `keys.export_requested` | info | `operation`, `expires_at` |
| `keys.exported` | warn | `classes`, `home_id`. `warn` because exporting key material is worth noticing. |
| `keys.export_rejected` | warn | `reason` (`invalid` \| `expired`). The confirmation store cannot tell an expired token from a consumed one, nor an unknown token from a foreign address, by design (data-model.md §6), so `expired` covers expired and consumed and `invalid` covers unknown and foreign. |

### 3.6 Network

| Event | Level | Additional fields |
|---|---|---|
| `network.state_changed` | info | `from`, `to`, `reason`. The derivation of `state` is in `data-model.md` §8.2. |
| `network.existing_detected` | warn | `node_count`. A stick carrying an unknown network; FR-018 auto-generation is blocked. |
| `network.reset_requested` | warn | `expires_at`. A confirmation token was issued; nothing has been reset. |
| `network.reset_started` | warn | `home_id_before`. The token was redeemed and the reset is under way. |
| `network.reset_completed` | warn | `home_id_before`, `duration_ms` |
| `network.reset_failed` | error | `reason` (`timeout` \| `driver_error` \| `firmware_update_in_progress`) |

**Not emitted**, deliberately, under Principle VIII (attention is finite): per-read events for
`GET /network` or `GET /keys` beyond `api.request`; command-queue depth sampling; confirmation
pruning counts; every ignored sidecar event. None of them has a symptom an operator can act on.

---

## 4. Audit trail versus event stream

The audit trail (`audit_entries`, FR-019) is the durable record of **configuration changes**. The
event stream is the operational record of **everything**. Where an action is audited, both records
are written and joined by `request_id`:

| Audit action | Event(s) emitted |
|---|---|
| `coordinator.selected` | `coordinator.selected` |
| `coordinator.rejected` | `coordinator.rejected` |
| `keys.replaced` | `keys.stored` (then `keys.applied` or `keys.apply_failed`) |
| `keys.generated` | `keys.stored` (then `keys.applied` or `keys.apply_failed`) |
| `keys.rejected` | `keys.in_use_rejected` when the reason is `key_in_use`; none for a validation failure, which is an `api.request` record with its 4xx status |
| `keys.apply_failed` | `keys.apply_failed` |
| `keys.export_requested` | `keys.export_requested` |
| `keys.exported` | `keys.exported` |
| `keys.export_rejected` | `keys.export_rejected` |
| `network.reset_requested` | `network.reset_requested` |
| `network.reset_rejected` | none: the refusal is an `api.request` record with its status code (404 or 410) |
| `network.reset` | `network.reset_started`, then `network.reset_completed` |
| `network.reset_failed` | `network.reset_started`, then `network.reset_failed` |

**Not audited**, by design: `driver.starting`, `driver.ready`, `driver.failed`, `driver.stopped`,
`driver.restart_paused`, `keys.corrupt`, `network.state_changed`, `coordinator.discovered`,
`coordinator.removed`, every `api.*` and `store.*` event.

Driver restarts are excluded on purpose: a crash loop would emit thousands of rows and bury the
operator's actual decisions, which is the opposite of what an audit trail is for. The event stream
carries them, and the audit rows that matter still name the outcome of the change that provoked
them.

---

## 5. Alerts

Three, each with a user-visible symptom and a runbook. They are derived from the stream, so an
operator with a log pipeline writes these rules once; the hub emits no alerting state of its own.

### 5.1 `coordinator_unreachable`

```text
condition: event == "network.state_changed" AND to == "faulted"
for:       2m        (i.e. no later state_changed with to != "faulted")
severity:  page
```

**Symptom the user sees**: the Z-Wave network is offline. Secure devices do not respond to any
command, and `GET /network` reports `state: faulted`.

**First action**: `docker compose logs` for the `driver.failed` events immediately before the
state change — the `reason` field names the cause. Runbook:
[`#runbook-coordinator-unreachable`](../quickstart.md#runbook-coordinator-unreachable).

### 5.2 `key_store_corrupt`

```text
condition: event == "keys.corrupt"
for:       0s        (one occurrence is enough)
severity:  page
```

**Symptom the user sees**: secure devices cannot be controlled, and `GET /network` reports
`ready_for_secure_inclusion: false` with `blocking_reasons[].code == "key_store_corrupt"`. Key reads
return `503`.

**First action**: do **not** regenerate. Restore `hub.db` or re-supply the original keys. Runbook:
[`#runbook-key-store-corrupt`](../quickstart.md#runbook-key-store-corrupt).

### 5.3 `driver_restart_loop`

```text
condition: event == "driver.failed"
count:     5 within 10m
severity:  page
```

**Symptom the user sees**: the Z-Wave network is unavailable, and it stays unavailable — the hub is
either retrying with backoff or has paused with `driver.restart_paused`.

**First action**: identify whether the device set is changing (flapping cable, failing hub) or
static (a stick that cannot be opened). Runbook:
[`#runbook-driver-restart-loop`](../quickstart.md#runbook-driver-restart-loop).

### 5.4 Considered and rejected

| Candidate | Why not |
|---|---|
| `keys_not_in_effect` | A transient state during a normal key change; it resolves in under 20 s or produces `driver.failed`, which already alerts. |
| `command_queue_full` | A client bug. The operator can only wait, and the client already got a `503` with a code. |
| `confirmation_expired` bursts | Noise from a mistyped token. Audited, not alerted. |
| `existing_network_detected` | A condition the operator deliberately created by attaching a provisioned stick. It appears in `GET /network`; it is not an incident. |

---

## 6. Schema versioning

`schema: 1` is fixed for this feature. The rules for a change:

- **Additive** (a new optional field on an existing event, or a new event name): no schema bump.
  Consumers must already ignore unknown fields.
- **Breaking** (renaming or removing a field, or changing a field's type or unit): bump `schema`
  and update this document in the same commit. `schema` appears on every event precisely so a
  consumer can filter rather than guess.
- Event names are never reused for a different meaning, even after removal, because a consumer's
  rule would silently change behaviour.
