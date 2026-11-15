# Tasks: USB Z-Wave Coordinator Discovery and Network Key Provisioning

**Input**: Design documents from `/specs/001-coordinator-key-provisioning/`

**Prerequisites**: plan.md, spec.md, research.md, data-model.md, contracts/openapi.yaml, contracts/zwave-server-sidecar.md, contracts/events.md, quickstart.md

**Tests**: Included. `plan.md` passes Constitution Principle V on an explicit test strategy (unit tests for pure logic, a real SQLite file and real HTTP handlers for boundaries, recorded sidecar payloads for what `--mock-driver` cannot produce), so the tests below are part of the task, not an optional extra. Test tasks come before the implementation they exercise.

**Organization**: Four user stories. US1 and US2 are both P1; US2 is the literal feature request, US1 is what makes it demonstrable.

## Format: `[ID] [P?] [Story] Description`

- **[P]**: Can run in parallel (different files, no dependency on an incomplete task)
- **[Story]**: Which user story this task belongs to (US1, US2, US3, US4)
- Every task names the exact file it creates or changes

## Path Conventions

Single Go module at the repository root. Application code in `cmd/zwave-hub/` and `internal/<package>/`, tests next to the package as `*_test.go`, sidecar protocol tests in `tests/contract/`, deployment assets in `deploy/`. No `src/` prefix.

---

## Phase 1: Setup (Shared Infrastructure)

**Purpose**: Module, dependencies and the command surface every later phase builds on

- [x] T001 Create the Go module and package skeleton in `go.mod`, `cmd/zwave-hub/main.go` and the `internal/{api,hub,coordinator,driver,store,event,config}` directories from plan.md §Source Code, with `go 1.27`
- [x] T002 [P] Add runtime dependencies to `go.mod`: `modernc.org/sqlite v1.62.0`, `github.com/coder/websocket v1.8.15`, `golang.org/x/sys`
- [x] T003 [P] Pin the radio sidecar in `sidecar/package.json` as `@zwave-js/server` 3.12.0 with `engines.node >= 20`, and commit the lockfile
- [x] T004 [P] Add `Makefile` targets `build`, `test`, `test-contract`, `lint`, `run-mock`, `image`, `up`, `rollback-check`; each target is the only way to run its action (constitution X)

---

## Phase 2: Foundational (Blocking Prerequisites)

**Purpose**: Configuration, events, storage, audit, the command queue and the HTTP boundary — shared by every story

**⚠️ CRITICAL**: No user story work can begin until this phase is complete

- [ ] T005 Implement configuration in `internal/config/config.go`: flags plus the eight `ZWAVE_HUB_*` variables from quickstart.md §2, base64 32-byte master-key validation, discovery-interval floor of 500 ms, `destructive_ops` parsing, fail-fast errors, and no environment reads outside this package (constitution III)
- [ ] T006 [P] Implement the emitter and event catalogue in `internal/event/event.go`: `log/slog` JSON handler only, typed events from `contracts/events.md`, allow-listed fields, and the never-send list enforced at construction (constitution VI)
- [ ] T007 [P] Write the initial schema in `internal/store/migrations/001_init.sql` exactly as data-model.md §10, including `PRAGMA user_version = 1`
- [ ] T008 Implement store open, pragmas and migration runner in `internal/store/store.go`: WAL, `synchronous=FULL`, `foreign_keys=ON` on every connection, `user_version` stepping, startup `PRAGMA integrity_check`
- [ ] T009 [P] Implement AES-256-GCM seal/open and master-key loading in `internal/store/crypto.go`: `ZWAVE_HUB_MASTER_KEY` else `<data>/master.key` created `0600`, 12-byte nonce, AAD `zwave-hub/key/v1|<class>`, blob `[0x01][nonce][ciphertext‖tag]`
- [ ] T010 [P] Implement the append-only audit writer and cursor pagination in `internal/store/audit.go`: one insert function, no update or delete path, action constants, `detail` allow-list from data-model.md §7
- [ ] T011 Implement the serialized command worker in `internal/hub/commands.go`: one goroutine over a FIFO channel of capacity 16, each command one SQLite transaction that also writes its audit row, snapshot published only after commit; a full queue returns `503 command_queue_full` with `Retry-After: 1` and writes nothing (R9, FR-021, SC-009)
- [ ] T012 [P] Implement RFC 9457 problem responses and the error-code mapping in `internal/api/problems.go`
- [ ] T013 Implement middleware in `internal/api/middleware.go`: request id, actor capture (remote address plus optional self-declared label, recorded unverified), panic recovery, one access event per request
- [ ] T014 Implement the router in `internal/api/router.go`: method-and-wildcard `ServeMux` under `/api/v1` plus `getHealth` at `/healthz` and `getReady` at `/readyz`, JSON content type, one mount point per resource for later phases
- [ ] T015 [P] Create `internal/api/presenters.go` as the single constructor of response bodies, with the redaction helper that refuses any field on the never-send list (FR-013, SC-004)
- [ ] T016 [P] Write store tests in `internal/store/store_test.go`: migrate from empty and from `user_version` 0, foreign keys enforced, integrity check, crypto round-trip and tamper detection
- [ ] T017 [P] Write command-queue tests in `internal/hub/commands_test.go`: arrival-order serialization under concurrent submissions, commit plus audit atomically, later-wins per class, no intermediate snapshot (FR-021, SC-009)

**Checkpoint**: Foundation ready — storage, audit, serialization and the HTTP boundary exist and pass their tests.

---

## Phase 3: User Story 1 — Discover and activate a plugged-in coordinator (Priority: P1) 🎯 MVP

**Goal**: A plugged-in stick is discovered, listed with an honest support verdict, selectable, connected, and reports its connection status and network membership.

**Independent Test**: With exactly one supported coordinator attached and the hub running, list coordinators through the API, select the one shown, and confirm the hub reports it as active and connected. No key work is needed for this to be demonstrable.

### Tests for User Story 1

> **NOTE: Write these tests first; they must fail before the implementation tasks run**

- [ ] T018 [P] [US1] Unit tests for hardware identity in `internal/coordinator/identity_test.go`: `usb:<vid>:<pid>:<serial>`, topology fallback, placeholder serials (`0001`, all zeros), `ambiguous_identity` for cloned serials (FR-004, R5, R14 §11)
- [ ] T019 [P] [US1] Unit tests for selection and switching in `internal/hub/coordinator_test.go`: unsupported rejected, an unverified device accepted as selectable, a selection detached when identification later reports it unsupported, switch allowed with no secure nodes, same `home_id` accepted, different or absent `home_id` rejected (FR-003)

### Implementation for User Story 1

- [ ] T020 [US1] Implement sysfs enumeration in `internal/coordinator/sysfs.go`: tty node → USB attributes through `/sys/class/tty`, reading `idVendor`, `idProduct`, `serial`, `manufacturer`, `product`
- [ ] T021 [US1] Implement identity derivation in `internal/coordinator/identity.go`: `usb:<vid>:<pid>:<serial>` or `usb:<vid>:<pid>:port:<topology>`, `identity_kind`, duplicate detection, `selectable` rule
- [ ] T022 [US1] Implement the discovery poller in `internal/coordinator/discover.go`: 2 s poll of `/sys/class/tty`, `/dev` cross-check, `unix.Access` on the resolved path to separate `permission_denied` from `not_passed_through`, `/dev/serial/by-id/...` preference, 300 ms debounce, snapshot publish
- [ ] T023 [US1] Implement the coordinator store in `internal/store/coordinators.go`: upsert by id, `support` transition rules, `last_path`, `last_seen_at`, list and get, plus the singleton provisioning row read/write for `home_id`, `key_revision` and `applied_revision`
- [ ] T024 [US1] Implement selection and switching in `internal/hub/coordinator.go`: commit through the command queue, reject unsupported devices, accept an unverified device and detach it if identification later reports it unsupported, enforce the FR-003 `home_id` gate, retain the selection when the device disappears
- [ ] T025 [US1] Implement CommonJS config rendering in `internal/driver/config.go`: `.cjs` under `/run/zwave-hub` mode `0600` with a random suffix, `storage.cacheDir`, `logConfig`, no security keys yet, unlink on stop
- [ ] T026 [US1] Implement the supervisor in `internal/driver/supervisor.go`: argv from `contracts/zwave-server-sidecar.md` §1, ephemeral port reservation, allow-listed environment, one child at a time, SIGTERM → 10 s → SIGKILL, never `driver.shutdown`, 30 s start deadline
- [ ] T027 [US1] Implement the WebSocket client in `internal/driver/client.go`: dial, retry on close `1013` every 500 ms until the deadline, `version` → `initialize` (schema 52, floor 47) → `start_listening`, `messageId` routing
- [ ] T028 [US1] Implement sidecar state mapping in `internal/driver/state.go`: `{driver, controller, nodes}` → `connection`, `home_id`, node count, secure node count, published to the hub snapshot (FR-005)
- [ ] T029 [US1] Implement coordinator adoption and identification in `internal/hub/identity.go`: merge discovery attributes with controller identity, move `unverified → supported|unsupported`, record firmware fields and a `support_reason` drawn from the FR-006 closed set
- [ ] T030 [US1] Implement coordinator handlers in `internal/api/coordinators.go`: `listCoordinators`, `getCoordinator`, `getActiveCoordinator`, `selectCoordinator`, with problem responses for every rejection
- [ ] T031 [US1] Implement the network status handler in `internal/api/network.go`: `getNetworkStatus` with state, coordinator connection and membership for the US1 scope
- [ ] T032 [US1] Implement the audit endpoint in `internal/api/audit.go`: `listAuditEntries` with cursor pagination by `id`, returning only allow-listed `detail`
- [ ] T033 [US1] Add coordinator and network response shapes to `internal/api/presenters.go`; no handler may construct a body directly
- [ ] T034 [US1] Wire the object graph in `cmd/zwave-hub/main.go`: flags and environment through `config`, store open and migrate, command worker, discovery, supervisor, HTTP server, signal handling, PID 1 behaviour
- [ ] T035 [P] [US1] Integration test in `internal/api/coordinators_test.go`: serve with no hardware (SC-008), empty list, API responsive; select a mock coordinator and reach `ready` within 30 seconds (SC-001); confirm `provisioning.home_id` is non-null once ready
- [ ] T036 [P] [US1] Contract test in `tests/contract/handshake_test.go`: a real `zwave-server --mock-driver` child exercises `1013` retry, `version`/`initialize`/`start_listening`, and schema negotiation against the floor

**Checkpoint**: US1 is fully functional and testable on its own. Deployable as the first increment.

---

## Phase 4: User Story 2 — Provision and manage network security keys (Priority: P1)

**Goal**: Classic keys can be submitted or generated, validated atomically, stored encrypted, applied through a driver-child restart, read as metadata only, exported through a two-step confirmation, and the network reports readiness for secure inclusion.

**Independent Test**: Starting from a selected coordinator and no configured keys, submit a complete key set (or request generation) and confirm the class is configured and the network ready for secure inclusion, then confirm the keys survive a hub restart.

### Tests for User Story 2

- [ ] T037 [P] [US2] Unit tests for key validation in `internal/hub/validate_test.go`: 32 hex characters / 16 bytes, atomic rejection naming the offending class, intra-set duplicate rejected, cross-set duplicate warned (FR-011, R14 §9)
- [ ] T038 [P] [US2] Unit tests for readiness in `internal/hub/readiness_test.go`: the predicate and all eight blocking reasons in the order of data-model.md §8.3, each carrying its fields (SC-002)
- [ ] T039 [P] [US2] Unit tests for confirmation tokens in `internal/store/confirmations_test.go`: hash-only storage, 120 s expiry, single use, operation- and actor-bound, prune older than 24 h
- [ ] T040 [P] [US2] Unit tests for key generation in `internal/hub/keygen_test.go`: classic set only, zero-node guard, all-zero redraw, Long Range never generated automatically (R15)

### Implementation for User Story 2

- [ ] T041 [P] [US2] Implement key validation in `internal/hub/validate.go`: 16-byte, 32-hex format for every class (FR-011), intra-set uniqueness, whole-submission rejection
- [ ] T042 [P] [US2] Implement key generation in `internal/hub/keygen.go`: `crypto/rand` 16-byte values, all-zero and all-`0xFF` redraw, classic and Long Range sets kept separate
- [ ] T043 [US2] Implement the key store in `internal/store/keys.go`: sealed rows, `key_revision` bump in the same transaction, `applied_revision` reads, decryption failure reported by class name and never repaired
- [ ] T044 [P] [US2] Implement the confirmation store in `internal/store/confirmations.go`: SHA-256 token hash as key, operation and actor binding, `issued_at + 120 s` expiry, consume once, hourly prune
- [ ] T045 [US2] Implement key domain operations in `internal/hub/keys.go`: submit a full set or one class, generate for selected classes, stored-versus-in-effect reporting, acceptance while the coordinator is faulted or absent (R14 §1 and §3), corrupt-store refusal with the three recovery paths, no in-use gate yet (that is US4), and the first-activation trigger: when a coordinator reaches `ready` with `key_revision == 0` and the controller reports zero nodes, generate the four classic keys and audit `keys.generated` before readiness may report true (FR-008, R15)
- [ ] T046 [US2] Implement readiness derivation in `internal/hub/readiness.go`: the four-class predicate and the ordered blocking-reason builder from data-model.md §8.3
- [ ] T047 [US2] Extend `internal/driver/config.go` with key rendering: `securityKeys` and `securityKeysLongRange` as `Buffer.from("<hex>","hex")`, omit absent classes rather than writing empty entries, same property names in both sets
- [ ] T048 [US2] Implement key application in `internal/driver/supervisor.go`: re-render the config from the database, restart only the child, set `applied_revision` when `start_listening` succeeds, persist the controller's `home_id` to `provisioning.home_id` on that same path, emit `keys.apply_failed` when it does not (FR-012, FR-015)
- [ ] T049 [US2] Implement key handlers in `internal/api/keys.go`: `getKeyConfiguration`, `submitKeys`, `generateKeys`
- [ ] T050 [US2] Implement the two-step export in `internal/api/confirmations.go`: `exportKeys` returns a token and no values first, then values with the token; unknown and wrong-actor tokens are `404 confirmation_invalid`; expired and consumed tokens are `410 confirmation_expired`, matching contracts/openapi.yaml:333; audit `keys.export_requested|exported|export_rejected`
- [ ] T051 [US2] Extend `internal/api/network.go` with readiness, blocking reasons and the `keys.in_effect` block
- [ ] T052 [US2] Add key response shapes and the export-only value path to `internal/api/presenters.go`, keeping every non-export body metadata-only
- [ ] T053 [US2] Register key audit actions in `internal/store/audit.go`, including a rejected-submission action so refused key work is attributable (FR-019, FR-021, SC-009)
- [ ] T054 [P] [US2] Integration test in `internal/api/keys_test.go`: submit, generate, invalid submission, single-class replacement, no coordinator attached, metadata-only reads (quickstart scenarios 4–7)
- [ ] T055 [P] [US2] Fixture test in `tests/contract/readiness_test.go`, with payloads under `tests/contract/fixtures/`: recorded `start_listening` payloads for the secure-node states the mock controller cannot produce
- [ ] T056 [P] [US2] Redaction test in `internal/api/redaction_test.go`: no key hex appears in any response, error, log record or audit row across all operations (SC-004)

**Checkpoint**: US1 and US2 both work; the network reports ready for secure inclusion.

---

## Phase 5: User Story 3 — Configuration survives restarts, unplugs, and re-plugs (Priority: P2)

**Goal**: A hub restart, an unplug, or a re-plug at a different path restores the same coordinator and ready state with no operator action.

**Independent Test**: Configure a coordinator and keys, restart the hub, and confirm the same coordinator is active and the network reports ready with no API calls re-applying anything. Then unplug and re-plug and confirm the same result.

### Tests for User Story 3

- [ ] T057 [P] [US3] Unit tests for restart backoff and the connection state machine in `internal/driver/supervisor_test.go`: five consecutive failures reach `faulted`, a device-set change resumes retries, `retrying` carries `next_retry_at`, no oscillation (R14 §7 and §8)
- [ ] T058 [P] [US3] Integration test in `internal/api/restart_test.go`: start with a stored configuration and confirm the selected coordinator and ready state return with zero API calls (SC-003, SC-006), and that `keys.in_effect` becomes true within 20 s p95 of a key change (plan.md §Performance Goals)

### Implementation for User Story 3

- [ ] T059 [US3] Implement boot-time restore in `internal/hub/coordinator.go`: load the provisioning record, match the discovered identity, start the driver only when the device is attached, report `awaiting_coordinator` otherwise (FR-015, FR-016)
- [ ] T060 [US3] Implement automatic adoption on re-plug in `internal/hub/coordinator.go`: the same identity at a new path returns to ready; an unknown device stays available without changing the selection (FR-004, FR-016)
- [ ] T061 [US3] Implement backoff and deadlines in `internal/driver/supervisor.go`: exponential backoff, 30 s start deadline, `faulted` after five failures with no device change, kill and record the last stderr line redacted (R14 §8)
- [ ] T062 [US3] Implement debounce and change notification in `internal/coordinator/discover.go`: 300 ms debounce, one change set per settle, notifying the supervisor to resume (R14 §7)
- [ ] T063 [US3] Implement disconnect and reconnect transitions in `internal/driver/state.go`: `ready → not_attached` on unplug, `not_attached → connecting → ready` on return, selection untouched throughout

**Checkpoint**: All three stories work independently; the hub survives restarts and physical disturbance.

---

## Phase 6: User Story 4 — Replace keys or reset the network deliberately (Priority: P3)

**Goal**: In-use keys are refused with the reset path named; a confirmed reset returns the hub and coordinator to unprovisioned; an unknown pre-existing network is reported, never overwritten.

**Independent Test**: With keys configured and a securely included device present, request a key replacement and confirm the hub rejects it and explains that a network reset is required; separately, confirm a reset returns the hub to the unprovisioned state with no keys and no network membership.

### Tests for User Story 4

- [x] T064 [P] [US4] Unit tests for the in-use gate in `internal/hub/keys_test.go`: per-class refusal while secure nodes exist, reset named in the reason, nothing written (FR-014)
- [x] T065 [P] [US4] Unit tests for the reset transition in `internal/hub/reset_test.go`: keys cleared, `home_id` NULL, `key_revision = 0`, `applied_revision` NULL, selection retained (FR-017, data-model.md §9.1)

### Implementation for User Story 4

- [ ] T066 [US4] Implement the in-use gate in `internal/hub/keys.go`: refuse replacement or regeneration for a class protecting included devices, name the reset path, audit the rejected outcome (FR-014, FR-021)
- [x] T067 [US4] Add `hard_reset` command support in `internal/driver/client.go`: send, never wait for a forwarded completion, expose the polled state that `internal/hub/reset.go` waits on (FR-017)
- [ ] T068 [US4] Implement reset orchestration in `internal/hub/reset.go`: one command that clears keys, `home_id` and revisions, calls the client's `hard_reset`, confirms completion by polling `controller.get_state`, audits `network.reset` or `network.reset_failed` (FR-017)
- [ ] T069 [US4] Implement unknown-network reporting in `internal/hub/readiness.go`: `existing_network_detected` with `home_id` and `node_count`, generation blocked, the two permitted paths reported (FR-018, R14 §10)
- [ ] T070 [US4] Implement the reset endpoint and confirmation in `internal/api/confirmations.go`: `resetNetwork` in two steps, and `503 destructive_ops_disabled` when `ZWAVE_HUB_DESTRUCTIVE_OPS=disabled`
- [ ] T071 [P] [US4] Integration test in `internal/api/reset_test.go`: refusal with secure nodes present, confirmed reset to unprovisioned, readiness false afterwards (quickstart scenario 10)

**Checkpoint**: All four stories complete; the dangerous path is gated and reversible only by the explicit operation.

---

## Phase 7: Polish & Cross-Cutting Concerns

**Purpose**: Deployment, measured performance, and the verification passes the constitution requires before this is done

- [ ] T072 [P] Add the multi-stage image in `deploy/Dockerfile`: `golang:1.27` build → `node:26-slim` runtime, sidecar installed at `/opt/zwave-sidecar`, non-root user, `/run/zwave-hub` tmpfs, USB access via `device_cgroup_rules` for char majors 188 and 166 with `group_add` for dialout
- [ ] T073 [P] Add `deploy/compose.yaml`: `/dev:/dev:rslave`, dialout group, data volume, tmpfs `/run`, configuration from `deploy/zwave-hub.env.example`
- [ ] T074 [P] Add `deploy/zwave-hub.env.example` with the eight variables and the recommended `ZWAVE_HUB_MASTER_KEY` placement from quickstart.md §7.4
- [ ] T075 [P] Add metadata-read benchmarks in `internal/api/bench_test.go` and record p95 for `GET /coordinators`, `GET /keys` and `GET /network` against the 100 ms target
- [ ] T076 Execute all ten scenarios in `specs/001-coordinator-key-provisioning/quickstart.md` §4 against the mock driver and, where a scenario requires it, real hardware; fix every discrepancy found
- [ ] T077 Run `make rollback-check`: previous image against the same data volume, `PRAGMA user_version` forward and back, confirm the previous binary ignores new columns (constitution VII)
- [ ] T078 Security hardening pass: child environment built from the allow-list only, `--disable-dns-sd` present, `ZWAVEJS_LOG_SECURITY_DETAILS` absent, `driver.shutdown` never sent, rendered config unlinked on stop
- [ ] T079 Audit completeness pass over `internal/hub/commands.go` and `internal/store/audit.go`: every mutating operation, and every refused one, writes exactly one audit row with time, actor, action and outcome; export and reset tokens never appear in logs or audit rows (FR-019, SC-007)
- [ ] T080 Reconcile `specs/001-coordinator-key-provisioning/data-model.md` §7 with the implemented audit action set, including the rejected-key action; add that action's row to `contracts/events.md` §4's audit-to-event pairing table and remove it from the not-audited list; reconcile §6 token rejection statuses with the clarified split (`410 confirmation_expired` for expired or used, `404 confirmation_invalid` for unknown or foreign address)
- [ ] T081 Run `make test`, `make test-contract` and `make lint`, and record the result in the completion report; no target may be skipped or weakened (constitution V, X)
- [ ] T082 [P] Add `.github/workflows/ci.yml` invoking `make build`, `make lint`, `make test` and `make test-contract` on pull requests and pushes to `main`, with no step logic outside the make targets (constitution X)
- [ ] T083 Run the three-alert drill from `quickstart.md` §8.2 — unplug the stick, corrupt one key row with `UPDATE security_keys SET key_blob = x'00'`, kill the sidecar in a loop — and confirm each alert fires with its runbook (constitution VIII, IX)

---

## Dependencies & Execution Order

### Phase Dependencies

- **Setup (Phase 1)**: No dependencies.
- **Foundational (Phase 2)**: Depends on Setup — blocks every user story.
- **User Stories (Phases 3–6)**: All depend on Foundational.
  - US1 can start immediately after Phase 2.
  - US2 depends on US1 at runtime (a selected coordinator, and the supervisor/client from T026–T028) and extends files US1 created.
  - US3 depends on US1 (supervisor, discovery, driver state) and needs US2 only to demonstrate readiness with keys.
  - US4 depends on US2 (key path, confirmations).
- **Polish (Phase 7)**: Depends on the stories being shipped.

### User Story Dependencies

Story independence here is **behavioral, not file-level**. Each story has its own independent test and can be validated without work from later stories, but successive stories add code to files an earlier story introduced:

| Story | Depends on | Files it extends |
|---|---|---|
| US1 (P1) | Foundational | — |
| US2 (P1) | US1 | `internal/driver/config.go`, `internal/driver/supervisor.go`, `internal/api/network.go`, `internal/api/presenters.go`, `internal/store/audit.go` |
| US3 (P2) | US1 | `internal/driver/supervisor.go`, `internal/driver/state.go`, `internal/coordinator/discover.go`, `internal/hub/coordinator.go` |
| US4 (P3) | US2 | `internal/hub/keys.go`, `internal/hub/readiness.go`, `internal/api/confirmations.go` |

Plan for the priority order in a single stream. A second developer can take US3's supervisor work as soon as T026 lands, and US2's pure key logic (T041–T046) as soon as Phase 2 is complete.

### Within Each User Story

- Tests are written first and fail before the implementation lands.
- Store adapters before domain logic, domain logic before the driver change, driver change before the HTTP handler, handler before wiring.
- A story is complete only when its independent test passes and its checkpoint is met.

### Parallel Opportunities

- Setup: T002–T004 run together once T001 exists.
- Foundational: T006, T007, T009, T010, T012, T015 can run together; T016 and T017 run together after the implementations they exercise.
- US1: T018 and T019 together; T035 and T036 together.
- US2: T037–T040 together; T041, T042 and T044 together; T054–T056 together.
- US3: T057 and T058 together.
- US4: T064, T065 and T071 together with nothing else in the story.
- Polish: T072–T075 together.

---

## Parallel Example: User Story 1

```bash
# Tests first, different files:
Task: "Unit tests for hardware identity in internal/coordinator/identity_test.go"
Task: "Unit tests for selection and switching in internal/hub/coordinator_test.go"

# After implementation, the two end-to-end checks:
Task: "Integration test in internal/api/coordinators_test.go"
Task: "Contract test in tests/contract/handshake_test.go"
```

## Parallel Example: User Story 2

```bash
# Four independent test files:
Task: "Unit tests for key validation in internal/hub/validate_test.go"
Task: "Unit tests for readiness in internal/hub/readiness_test.go"
Task: "Unit tests for confirmation tokens in internal/store/confirmations_test.go"
Task: "Unit tests for key generation in internal/hub/keygen_test.go"

# Three independent implementation files:
Task: "Implement key validation in internal/hub/validate.go"
Task: "Implement key generation in internal/hub/keygen.go"
Task: "Implement the confirmation store in internal/store/confirmations.go"
```

## Parallel Example: User Story 3

```bash
Task: "Unit tests for restart backoff in internal/driver/supervisor_test.go"
Task: "Integration test in internal/api/restart_test.go"
```

## Parallel Example: User Story 4

```bash
Task: "Unit tests for the in-use gate in internal/hub/keys_test.go"
Task: "Unit tests for the reset transition in internal/hub/reset_test.go"
Task: "Integration test in internal/api/reset_test.go"
```

---

## Implementation Strategy

### MVP First (User Stories 1 and 2)

1. Complete Phase 1: Setup.
2. Complete Phase 2: Foundational — it blocks everything.
3. Complete Phase 3 (US1) and Phase 4 (US2).
4. **STOP and VALIDATE**: run the independent tests for both stories; the hub can discover, select and provision a network.
5. Deploy or demo.

User Story 1 alone (19 tasks) is the smallest deployable increment, but it only proves hardware recognition. User Story 2 is the literal feature request and is also P1, so the feature is not demonstrable without it. User Stories 3 and 4 are P2 and P3 and ship after.

### Incremental Delivery

1. Setup + Foundational → foundation ready.
2. US1 → the stick is listed, selected and connected → deploy/demo.
3. US2 → keys are provisioned, applied and exported → deploy/demo.
4. US3 → restarts and re-plugs are invisible to the operator → deploy/demo.
5. US4 → key replacement is refused safely and reset is explicit → deploy/demo.
6. Polish → image, compose file, benchmarks, the CI workflow, the alert drill and the verification passes.

Each step leaves the previous behavior intact.

### Parallel Team Strategy

1. One person completes Setup + Foundational; the command queue and schema are the shared contract.
2. Once Foundational is done:
   - Developer A: US1.
   - Developer B: US2's pure key logic (T041–T046) in parallel, then the driver and HTTP wiring after T026–T028 land.
   - Developer C: US3's supervisor work after T026, then discovery debounce.
3. US4 starts once US2 is on the branch.
4. Shared files (`presenters.go`, `router.go`, `network.go`, `supervisor.go`, `audit.go`) are edited by one owner at a time; coordinate before touching them.

---

## Notes

- The `[P]` marker means different files and no dependency on an incomplete task; it does not survive a shared file.
- T016 depends on T008–T009, and T017 on T011, despite being marked parallel with each other.
- T053 adds a rejected-submission audit action that data-model.md §7 does not yet list; T080 reconciles the design doc rather than leaving the code ahead of it.
- `checklists/security.md` remains unchecked. CHK011, CHK013, CHK016 and CHK026 identify real spec gaps that the plan answers (data-model.md §8.3, research.md R14/R15); the tasks implement the plan's answers; the 2026-11-13 clarification session absorbed several of them into `spec.md`, and the reviewer decides which remain.
- The ten quickstart scenarios are the acceptance walk-through; T076 runs all ten, not a sample.
- Commit after each task or logical group; stop at a checkpoint to validate the story on its own.
- Tasks T003 and T026–T027 carry the Principle I deviation recorded in `plan.md` §Complexity Tracking: the `zwave-server` child is a second process inside the same container, justified because zwave-js is the requested integration point and no Go implementation of the Serial API or S2 cryptography exists. That justification travels with those tasks into review (constitution, Development Workflow §3).
- The 2026-11-13 clarification session added testable clauses that these tasks implement: FR-004's topology caveat (T018, T021), FR-008's zero-node guard (T040, T045), FR-011's 16-byte/32-hex format (T041), FR-013's 120-second single-use token and unusable-versus-invalid split (T044, T050), and driver-based support determination (T020–T022). Its later pass added three more: the tri-state support verdict with an unverified device selectable and detached on a failed identification (T019, T024, T029), refused key work being audited (T053, T066, T079), and the FR-017 reset post-conditions (T065, T068, T071).
- `ZWAVE_HUB_DESTRUCTIVE_OPS` gates `resetNetwork` only in the contract, the env example and T070; `plan.md:43` also claims it gates export. The narrower reading is implemented here — decide before implementation whether export should be gated (INC-4).
