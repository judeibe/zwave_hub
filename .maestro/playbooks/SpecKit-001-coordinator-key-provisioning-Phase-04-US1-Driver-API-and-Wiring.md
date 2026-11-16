# Phase 04: User Story 1 — Supervisor, sidecar client, API and wiring (Priority: P1) 🎯 MVP

Second half of the MVP: the sidecar config renderer, supervisor, WebSocket client and state mapping, the coordinator, network and audit handlers, the wired `main`, and the integration and contract tests. At the end a plugged-in (or mock) stick is listed, selectable, connected and reports its network membership.

## Spec Kit Context

- **Spec Kit phase:** Phase 3b of tasks.md (this file is execution phase 04 of 09)
- **Feature:** 001-coordinator-key-provisioning
- **Specification:** specs/001-coordinator-key-provisioning/spec.md (note: this feature's spec file is `spec.md`, not `specification.md`)
- **Plan:** specs/001-coordinator-key-provisioning/plan.md
- **Task source:** specs/001-coordinator-key-provisioning/tasks.md (task IDs and `[US#]` markers below are copied verbatim)
- **Design documents:** specs/001-coordinator-key-provisioning/research.md, data-model.md, quickstart.md, contracts/openapi.yaml, contracts/zwave-server-sidecar.md, contracts/events.md
- **Constitution:** .specify/memory/constitution.md (Principles I–X are binding; the Principle I deviation for the `zwave-server` child is recorded in plan.md §Complexity Tracking)

## Execution Rules

Work through this document without asking the user anything. If something is ambiguous, follow the design documents above, record the choice in the task's commit message, and continue.

1. **Sequential tasks** (no `[P]`) run in listed order, one at a time, by you.
2. **Parallel tasks** (`[P]`) that appear next to each other form a *parallel batch*. Delegate **each `[P]` task in a batch to its own sub-agent** (use the `subagent` tool, agent `general`), launching every sub-agent in the batch in a single step so they run concurrently, and wait for all of them before starting the next task or batch. A sub-agent prompt must be self-contained: give it the full task line, the Spec Kit Context paths above, the Go module path and package layout from plan.md §Source Code, and the instruction to create or edit **only the file(s) named in that task**. Do not let two sub-agents touch the same file; if they would, run those tasks sequentially instead.
3. After a batch returns, review each sub-agent's result, run `gofmt` and `go vet` on the touched packages, fix integration problems yourself, then tick the checkboxes.
4. Tests listed before an implementation are written first and should fail before the implementation lands.
5. Tick a task's checkbox (`- [x]`) only when its file exists, builds, and its stated behaviour is verified. Never tick a task that you skipped or weakened.
6. Search the repository for an existing helper before writing a new one. Constitution III: constructor injection, no globals, no `init()`, no environment reads outside `internal/config`. Constitution VI: events only through `internal/event`; never log key material.
7. Commit after each task or logical group, but do not push.

## Platform Notes

- Development host is macOS arm64; production target is Linux. Sysfs discovery is Linux-specific, so make the sysfs root an injected path and test against a fixture tree under `testdata/`. Real-device checks run only in the Docker image on Linux.
- The Go toolchain was not installed when these documents were generated. Phase 01 verifies and installs it.

## Parallel Batches

Delegate each task in a batch to its own sub-agent, concurrently:

- T035, T036

## Tasks

- [x] T025 [US1] Implement CommonJS config rendering in `internal/driver/config.go`: `.cjs` under `/run/zwave-hub` mode `0600` with a random suffix, `storage.cacheDir`, `logConfig`, no security keys yet, unlink on stop
- [x] T026 [US1] Implement the supervisor in `internal/driver/supervisor.go`: argv from `contracts/zwave-server-sidecar.md` §1, ephemeral port reservation, allow-listed environment, one child at a time, SIGTERM → 10 s → SIGKILL, never `driver.shutdown`, 30 s start deadline
- [x] T027 [US1] Implement the WebSocket client in `internal/driver/client.go`: dial, retry on close `1013` every 500 ms until the deadline, `version` → `initialize` (schema 52, floor 47) → `start_listening`, `messageId` routing
- [x] T028 [US1] Implement sidecar state mapping in `internal/driver/state.go`: `{driver, controller, nodes}` → `connection`, `home_id`, node count, secure node count, published to the hub snapshot (FR-005)
- [x] T029 [US1] Implement coordinator adoption and identification in `internal/hub/identity.go`: merge discovery attributes with controller identity, move `unverified → supported|unsupported`, record firmware fields and a `support_reason` drawn from the FR-006 closed set
- [x] T030 [US1] Implement coordinator handlers in `internal/api/coordinators.go`: `listCoordinators`, `getCoordinator`, `getActiveCoordinator`, `selectCoordinator`, with problem responses for every rejection
- [x] T031 [US1] Implement the network status handler in `internal/api/network.go`: `getNetworkStatus` with state, coordinator connection and membership for the US1 scope
- [x] T032 [US1] Implement the audit endpoint in `internal/api/audit.go`: `listAuditEntries` with cursor pagination by `id`, returning only allow-listed `detail`
- [x] T033 [US1] Add coordinator and network response shapes to `internal/api/presenters.go`; no handler may construct a body directly
- [x] T034 [US1] Wire the object graph in `cmd/zwave-hub/main.go`: flags and environment through `config`, store open and migrate, command worker, discovery, supervisor, HTTP server, signal handling, PID 1 behaviour
- [x] T035 [P] [US1] Integration test in `internal/api/coordinators_test.go`: serve with no hardware (SC-008), empty list, API responsive; select a mock coordinator and reach `ready` within 30 seconds (SC-001); confirm `provisioning.home_id` is non-null once ready
- [x] T036 [P] [US1] Contract test in `tests/contract/handshake_test.go`: a real `zwave-server --mock-driver` child exercises `1013` retry, `version`/`initialize`/`start_listening`, and schema negotiation against the floor

## Completion

- [x] `go test ./...` passes, including `internal/api/coordinators_test.go` (no-hardware start, SC-008; ready within 30 s, SC-001).
- [x] `make test-contract` passes against a real `zwave-server --mock-driver` child (`npm ci` in `sidecar/` first).
- [x] US1 independent test: run the hub against the mock driver, `GET /api/v1/coordinators`, `PUT /api/v1/active-coordinator`, then `GET /api/v1/network` shows the coordinator `ready`.
- [x] Verify all changes work as expected
- [x] Run `/speckit.analyze` to verify consistency

## Manual Follow-Up (not executed by Auto Run)

- Real-hardware check on Linux/Docker: plug a physical USB Z-Wave stick into a Linux host and confirm it is listed, selectable and reaches `ready` (SC-001). This run used `--mock-driver` on macOS, where sysfs discovery does not exist, so the real sysfs path and the real serial path to the sidecar are unexercised.
- Decide whether the `ambiguous_identity` support reason should get its own problem code in `contracts/openapi.yaml`. It is currently reported as `coordinator_unsupported` (409) with a detail naming the cause.
- `GET /api/v1/active-coordinator` with nothing selected returns 404 `no_active_coordinator`, as the contract says; the code's default for that code is 409. Confirm the contract is the intended source of truth.
- `tasks.md` Phase 1–3 and Phase 04 checkboxes were not updated; the playbooks track progress instead.
