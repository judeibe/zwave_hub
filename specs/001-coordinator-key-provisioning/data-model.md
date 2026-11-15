# Phase 1 Data Model: USB Z-Wave Coordinator Discovery and Network Key Provisioning

**Feature**: `001-coordinator-key-provisioning` | **Date**: 2026-11-12 | **Plan**: [plan.md](./plan.md)

The hub is the source of truth for everything except the radio's own network data. Seven stored entities and one in-memory entity are enough; every field below is justified by a functional requirement, and nothing is stored speculatively.

---

## 1. Entity overview

| Entity | Lifetime | Storage | Requirement |
|---|---|---|---|
| Coordinator | Durable; one row per device ever seen | `coordinators` | FR-002, FR-004, FR-005, FR-018 |
| Security Key | Durable; one row per configured class | `security_keys` | FR-007 to FR-014 |
| Provisioning Record | Durable; exactly one row | `provisioning` | FR-003, FR-008, FR-018 |
| Confirmation | Durable until expiry + 24 h | `confirmations` | FR-013, FR-017 |
| Audit Entry | Durable, append-only | `audit_entries` | FR-019, FR-021 |
| Discovery Snapshot | In memory, rebuilt every 2 s | none | FR-001, FR-006 |
| Driver Runtime | In memory, owned by the supervisor | none | FR-005, FR-012 |
| Readiness View | Derived on read from the above | none | FR-008, FR-010 |

```text
Coordinator 1 ──── 0..1 Provisioning Record (active_coordinator_id)
Coordinator 1 ──── 0..n Audit Entry     (coordinator_id, nullable)
Security Key 0..6 ─── 1 Provisioning Record (via key_revision, no FK)
Confirmation 0..n ─── 0..1 Audit Entry  (linked by request_id)
```

The key set is deliberately *not* related to the coordinator by a foreign key. Keys belong to the hub's network, not to a physical stick; the provisioning record is what binds them, and that binding is exactly what FR-003 lets an operator change.

---

## 2. Coordinator

A device the hub has discovered. Rows persist after the device is unplugged so identity, support verdict and the last known path survive.

| Field | Type | Constraints | Notes |
|---|---|---|---|
| `id` | text | primary key | `usb:<vid>:<pid>:<serial>` or `usb:<vid>:<pid>:port:<topology>`. Stable across restarts; used verbatim in URLs and audit rows. |
| `usb_vid` | text | 4 lowercase hex | From sysfs `idVendor`. |
| `usb_pid` | text | 4 lowercase hex | From sysfs `idProduct`. |
| `usb_serial` | text | nullable | From sysfs `serial`; `NULL` when absent or a known placeholder (`0001`, all zeros). |
| `identity_kind` | text | `serial` \| `topology` | Tells the operator whether the selection survives a port change (FR-004 caveat). |
| `manufacturer` | text | nullable | sysfs `manufacturer`. |
| `product` | text | nullable | sysfs `product`. |
| `firmware_version` | text | nullable | Controller `firmwareVersion` after identification. |
| `sdk_version` | text | nullable | Controller `sdkVersion`. |
| `zwave_manufacturer_id` | integer | nullable | Controller `manufacturerId`. |
| `zwave_product_type` | integer | nullable | Controller `productType`. |
| `zwave_product_id` | integer | nullable | Controller `productId`. |
| `zwave_chip_type` | text | nullable | Controller `zwaveChipType` (schema 47+). |
| `support` | text | `supported` \| `unsupported` \| `unverified` | `unverified` until the driver has identified the device. |
| `support_reason` | text | nullable | One of the reason codes in §7. |
| `last_path` | text | nullable | Preferred `/dev/serial/by-id/...` link, else `/dev/tty*`. |
| `first_seen_at` | text | RFC 3339 UTC | |
| `last_seen_at` | text | RFC 3339 UTC | Updated on every discovery poll that sees the device. |

**Transient fields** (never stored, merged in from the discovery snapshot and the driver): `attached`, `path`, `path_kind` (`by_id` \| `tty`), `connection`, `selectable`, `network` (home ID, node count, secure node count — present only while `connection == ready`).

**Validation rules**
- `id` is derived, never supplied by a client. A client-supplied id that does not match a discovered device is `404 coordinator_not_found`.
- `support` only ever moves `unverified → supported` or `unverified → unsupported`. A `supported` verdict is never downgraded by a later failed open (that is a connection fault, not a support change); it *is* downgraded if a different device appears at the same identity with a non-Z-Wave identity.
- `selectable` is `false` for `unsupported`, for `ambiguous_identity`, and for a device that is not attached.

---

## 3. Security Key

One row per configured class. A class with no row is "not configured" — absence is the representation, not a `NULL` value.

| Field | Type | Constraints | Notes |
|---|---|---|---|
| `security_class` | text | primary key, one of six | See §4. |
| `key_blob` | blob | not null, ≥ 29 bytes | `[0x01][12-byte nonce][ciphertext‖tag]`, AES-256-GCM, AAD = `zwave-hub/key/v1\|<class>`. |
| `key_source` | text | `generated` \| `supplied` | FR-013 metadata. |
| `created_at` | text | RFC 3339 UTC | First time this class was configured. |
| `changed_at` | text | RFC 3339 UTC | Last change; FR-013 metadata. |

**Invariants**
- A key value is 16 bytes (32 hex characters) before encryption. Enforced in `hub/validate.go` and re-checked on decryption.
- No two configured classes *within* the classic set share a value, and no two within the Long Range set share a value (zwave-js refuses such a driver with `Driver_InvalidOptions`). Duplication *across* the two sets is permitted and reported as a warning.
- A row is only written as part of a transaction that also bumps `provisioning.key_revision` and inserts the audit row.
- Decryption failure is reported, never repaired: the row is left untouched so a backup restore can still recover it.

---

## 4. Security classes

| API name | Config object | Config property | Readiness baseline |
|---|---|---|---|
| `S2_AccessControl` | `securityKeys` | `S2_AccessControl` | yes |
| `S2_Authenticated` | `securityKeys` | `S2_Authenticated` | yes |
| `S2_Unauthenticated` | `securityKeys` | `S2_Unauthenticated` | yes |
| `S0_Legacy` | `securityKeys` | `S0_Legacy` | yes |
| `S2_Authenticated_LR` | `securityKeysLongRange` | `S2_Authenticated` | no (opt-in) |
| `S2_AccessControl_LR` | `securityKeysLongRange` | `S2_AccessControl` | no (opt-in) |

The four classic classes form the base set (FR-007); readiness for secure inclusion is judged on them alone, and the Long Range variants exist only once provisioned (FR-008, SC-002).

---

## 5. Provisioning Record

Exactly one row. It answers "which stick, which network, which key revision is in effect".

| Field | Type | Constraints | Notes |
|---|---|---|---|
| `id` | integer | primary key, `CHECK (id = 1)` | Singleton. |
| `active_coordinator_id` | text | nullable, FK → `coordinators.id` | `NULL` before any selection. |
| `home_id` | text | nullable, `0x` + 8 lowercase hex | The Z-Wave network identifier. `NULL` until a coordinator is ready. |
| `key_revision` | integer | not null, default 0 | Bumped once per accepted key change. 0 means no keys. |
| `applied_revision` | integer | nullable | Revision the running driver was started with. `NULL` = never applied. |
| `updated_at` | text | RFC 3339 UTC | |

**Derived values**
- `keys.in_effect = (applied_revision == key_revision) && key_revision > 0`.
- `network_state` (see §8).
- `existing_network_detected = home_id != null && key_revision == 0 && node_count > 0` — the FR-018 state.

---

## 6. Confirmation

One row per issued token. Shared by the backup export (FR-013) and the network reset (FR-017).

| Field | Type | Constraints | Notes |
|---|---|---|---|
| `token_hash` | text | primary key | SHA-256 hex of the token. The token itself is never stored. |
| `operation` | text | `keys.export` \| `network.reset` | A token is only valid for the operation it was issued for. |
| `actor_addr` | text | not null | The address that requested it; a token presented from another address is `confirmation_invalid`. |
| `issued_at` | text | RFC 3339 UTC | |
| `expires_at` | text | RFC 3339 UTC | `issued_at + 120 s`. |
| `consumed_at` | text | nullable | Set when the operation runs; a consumed token is never valid again. |

Rows older than 24 hours are deleted on startup and once per hour. A token presented after deletion is `confirmation_invalid` (404), which is indistinguishable from a token that never existed — the intended behaviour.

---

## 7. Audit Entry

Append-only. No update path and no delete path exists in the code; the table is written by exactly one function.

| Field | Type | Constraints | Notes |
|---|---|---|---|
| `id` | integer | primary key autoincrement | Monotonic; also the pagination cursor. |
| `at` | text | RFC 3339 UTC with milliseconds | |
| `action` | text | enum below | |
| `actor_addr` | text | not null | Requesting client's network address (FR-019). |
| `actor_label` | text | nullable, ≤ 64 chars | Self-declared, recorded as unverified. |
| `request_id` | text | not null | Correlates the audit row with events and the response. |
| `coordinator_id` | text | nullable | |
| `security_class` | text | nullable | Set for single-class actions. |
| `outcome` | text | `succeeded` \| `failed` \| `rejected` | |
| `detail` | text | JSON object, default `{}` | Allow-listed keys only: `classes`, `source`, `reason`, `expected_home_id`, `actual_home_id`, `cleared`. Never key material, never a rendered config fragment. |

**Actions**

| Action | Written when | Outcome values |
|---|---|---|
| `coordinator.selected` | a selection is committed | `succeeded` |
| `coordinator.rejected` | a selection is refused | `rejected` |
| `keys.replaced` | a submission is committed | `succeeded` |
| `keys.generated` | generated keys are committed | `succeeded` |
| `keys.rejected` | a submission or a generation is refused — a class in use by securely included devices, or a validation failure | `rejected` |
| `keys.apply_failed` | the driver did not come back with the new revision | `failed` |
| `keys.export_requested` | a confirmation token is issued | `succeeded` |
| `keys.exported` | the confirmed export returns values | `succeeded` |
| `keys.export_rejected` | a bad, expired or consumed token | `rejected` |
| `network.reset_requested` | a confirmation token is issued | `succeeded` |
| `network.reset_rejected` | a reset is presented with a bad, expired or consumed token | `rejected` |
| `network.reset` | the reset completes | `succeeded` |
| `network.reset_failed` | the driver reset fails | `failed` |

Driver lifecycle (`driver.starting`, `driver.ready`, `driver.failed`) is **not** audited — it is an operational event, not a configuration change. It goes to the event stream only, which keeps the audit trail readable and stops a crash loop from burying real operator actions.

---

## 8. Derived views

### 8.1 Coordinator connection (FR-005)

| Value | Meaning |
|---|---|
| `not_attached` | No discovered device matches the selected identity. |
| `connecting` | A driver child is starting for this device. |
| `ready` | `start_listening` succeeded and the controller interview completed. |
| `retrying` | The last attempt failed and another is scheduled; `next_retry_at` is set. |
| `faulted` | Retrying has paused — permission denied, unsupported device, or five consecutive failures with no change in the device set. |

### 8.2 Network state

| State | Condition |
|---|---|
| `unprovisioned` | No active coordinator, or no network on record at all: `home_id` is null and `key_revision == 0`. A confirmed reset lands here (FR-017), and the coordinator selection is kept. |
| `awaiting_coordinator` | An active coordinator exists but `connection != ready`. |
| `degraded` | `connection == ready` but readiness for secure inclusion is false. |
| `ready` | `connection == ready` and readiness for secure inclusion is true. |
| `faulted` | `connection == faulted`, or the key store is corrupt, or `existing_network_detected`. |

`degraded` is where "keys stored but not in effect" lives; the `keys` block carries `in_effect: false` and the reason, so the state alone never has to encode which condition applies.

### 8.3 Readiness for secure device inclusion (FR-008, SC-002)

```text
ready_for_secure_inclusion =
      key_store.state == "ok"
   && coordinator.connection == "ready"
   && home_id != null
   && all four classic classes have a row
   && applied_revision == key_revision
```

Blocking reasons, emitted in this order, each with the fields a client needs to act:

| Code | Extra fields | Meaning |
|---|---|---|
| `key_store_corrupt` | `classes` | One or more key rows failed authentication. |
| `no_active_coordinator` | — | Nothing selected. |
| `coordinator_not_attached` | `last_path` | Selected device is not present. |
| `coordinator_faulted` | `reason` | Present but unusable. |
| `network_mismatch` | `expected_home_id`, `actual_home_id` | Attached stick belongs to a different network. |
| `existing_network_detected` | `home_id`, `node_count` | Unknown network with nodes; FR-018. |
| `keys_missing` | `classes` | One or more classic classes unconfigured. |
| `keys_not_in_effect` | `key_revision`, `applied_revision` | Stored revision is not running yet. |

The list is exhaustive by construction: the code that builds it is the same code that evaluates the predicate, so a client that sees `ready_for_secure_inclusion: false` always gets at least one reason.

---

## 9. State transitions

### 9.1 Key revision lifecycle

| From | Event | To | Notes |
|---|---|---|---|
| `key_revision = n`, applied `n` | keys submitted, no coordinator ready | stored `n+1`, applied `n` | `in_effect: false`, reason `coordinator_unavailable`. |
| stored `n+1`, applied `n` | driver starts successfully | applied `n+1` | Re-rendered from the database; no reconciliation pass. |
| stored `n+1`, applied `n` | driver fails to start | unchanged | `keys.apply_failed` audited; backoff continues. |
| any | confirmed reset | `key_revision = 0`, applied `NULL`, `home_id = NULL`, all key rows deleted | Provisioning record returns to `unprovisioned`. |

### 9.2 Selection lifecycle

| From | Event | To |
|---|---|---|
| none | `PUT /active-coordinator` on a supported or unverified attached device | selection committed, driver started |
| selected A | `PUT /active-coordinator` with B, no secure nodes and no key revision | B selected (keys retained, `in_effect` recomputed) |
| selected A | `PUT /active-coordinator` with B, secure nodes present, B reports the stored `home_id` | B selected |
| selected A | `PUT /active-coordinator` with B, secure nodes present, B reports a different or absent `home_id` | rejected `coordinator_network_mismatch`, A unchanged |
| selected A | A disappears | connection `not_attached`, selection retained (FR-004) |
| selected A | A reappears at a different path | connection returns to `ready`, selection retained |

### 9.3 Coordinator connection

```text
not_attached ──device appears──▶ connecting ──ready──▶ ready
                                     │                  │
                                     └──failure──▶ retrying ──attempt──▶ connecting
                                                       │
                                                       └──5 failures, no device change──▶ faulted
ready ──device disappears──▶ not_attached
ready ──child exits──▶ retrying
faulted ──device set changes──▶ connecting
```

---

## 10. Physical schema

```sql
PRAGMA user_version = 1;

CREATE TABLE coordinators (
  id                    TEXT PRIMARY KEY,
  usb_vid               TEXT NOT NULL,
  usb_pid               TEXT NOT NULL,
  usb_serial            TEXT,
  identity_kind         TEXT NOT NULL CHECK (identity_kind IN ('serial','topology')),
  manufacturer          TEXT,
  product               TEXT,
  firmware_version      TEXT,
  sdk_version           TEXT,
  zwave_manufacturer_id INTEGER,
  zwave_product_type    INTEGER,
  zwave_product_id      INTEGER,
  zwave_chip_type       TEXT,
  support               TEXT NOT NULL CHECK (support IN ('supported','unsupported','unverified')),
  support_reason        TEXT,
  last_path             TEXT,
  first_seen_at         TEXT NOT NULL,
  last_seen_at          TEXT NOT NULL
);

CREATE TABLE provisioning (
  id                    INTEGER PRIMARY KEY CHECK (id = 1),
  active_coordinator_id TEXT REFERENCES coordinators(id) ON DELETE RESTRICT,
  home_id               TEXT,
  key_revision          INTEGER NOT NULL DEFAULT 0,
  applied_revision      INTEGER,
  updated_at            TEXT NOT NULL
);

CREATE TABLE security_keys (
  security_class TEXT PRIMARY KEY CHECK (security_class IN (
                   'S2_AccessControl','S2_Authenticated','S2_Unauthenticated','S0_Legacy',
                   'S2_Authenticated_LR','S2_AccessControl_LR')),
  key_blob       BLOB NOT NULL,
  key_source     TEXT NOT NULL CHECK (key_source IN ('generated','supplied')),
  created_at     TEXT NOT NULL,
  changed_at     TEXT NOT NULL
);

CREATE TABLE confirmations (
  token_hash  TEXT PRIMARY KEY,
  operation   TEXT NOT NULL CHECK (operation IN ('keys.export','network.reset')),
  actor_addr  TEXT NOT NULL,
  issued_at   TEXT NOT NULL,
  expires_at  TEXT NOT NULL,
  consumed_at TEXT
);
CREATE INDEX confirmations_expires_at ON confirmations(expires_at);

CREATE TABLE audit_entries (
  id             INTEGER PRIMARY KEY AUTOINCREMENT,
  at             TEXT NOT NULL,
  action         TEXT NOT NULL,
  actor_addr     TEXT NOT NULL,
  actor_label    TEXT,
  request_id     TEXT NOT NULL,
  coordinator_id TEXT,
  security_class TEXT,
  outcome        TEXT NOT NULL CHECK (outcome IN ('succeeded','failed','rejected')),
  detail         TEXT NOT NULL DEFAULT '{}'
);
CREATE INDEX audit_entries_at ON audit_entries(at DESC, id DESC);
CREATE INDEX audit_entries_action ON audit_entries(action, at DESC);

INSERT INTO provisioning (id, updated_at) VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ','now'));
```

**Notes on the schema**
- Timestamps are text, not integers: they are read by humans in `sqlite3` and compared lexicographically, which is correct for RFC 3339 UTC. No arithmetic is done on them.
- `home_id` is text (`0x` + 8 hex) because a 32-bit ID can exceed `int32` and SQLite's dynamic typing would silently store it as a float in some paths. Identifiers stay identifiers.
- `PRAGMA foreign_keys=ON` is set on every connection; `ON DELETE RESTRICT` makes it impossible to drop a coordinator that an audit row or the provisioning record still names.
- Migrations are additive. A rollback ships the previous binary, which ignores columns it does not know about; no migration in this feature is destructive.

---

## 11. Validation rules traceability

| Rule | Enforced in | Requirement |
|---|---|---|
| Key is exactly 32 hex characters (16 bytes) | `hub/validate.go`, before any write | FR-011 |
| A malformed value rejects the whole submission | single transaction; nothing written | FR-011, SC-005 |
| No duplicate value within the classic set or within the LR set | `hub/validate.go` | FR-011, edge case 9 |
| A class in use cannot be replaced or regenerated | `hub/keys.go` gate on `secure_node_count > 0` | FR-014 |
| Readiness needs the four classic classes only | `hub/readiness.go` | FR-007, FR-008, SC-002 |
| Key values never appear outside the confirmed export | `api/presenters.go` is the only constructor of response bodies; `event` fields are allow-listed | FR-013, SC-004 |
| Reset requires confirmation and clears keys and network record | `hub/reset.go` | FR-017 |
| Selection binds to identity, not path | `coordinator/identity.go` | FR-004 |
| A replacement coordinator must report the stored network identifier | `hub/coordinator.go` | FR-003 |
| Every configuration change is audited with actor and outcome | `hub/commands.go` wraps every mutating command | FR-019 |
| Changes are applied in arrival order, atomically | single command worker + snapshot publish | FR-021, SC-009 |

---

## 12. Sizing and retention

| Table | Rows in steady state | Growth |
|---|---|---|
| `coordinators` | 1–3 | one per distinct device ever attached |
| `provisioning` | 1 | fixed |
| `security_keys` | 0–6 | fixed |
| `confirmations` | 0–2 | pruned hourly, 24 h retention |
| `audit_entries` | tens | tens per year; retained indefinitely, read through a cursor-paginated endpoint |

The whole database stays well under 1 MB in normal operation, which is why `synchronous=FULL` is affordable and why there is no archive or compaction task.
