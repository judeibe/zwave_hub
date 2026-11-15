# Phase 07: User Story 3 — Configuration survives restarts, unplugs and re-plugs (Priority: P2)

Makes the hub restore the same coordinator and ready state after a restart, an unplug, or a re-plug at a different path, with backoff, deadlines and debounce so the status settles rather than oscillates.

## Spec Kit Context

- **Spec Kit phase:** Phase 5 of tasks.md (this file is execution phase 07 of 09)
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

## Parallel Batches

Delegate each task in a batch to its own sub-agent, concurrently:

- T057, T058

## Tasks

- [x] T057 [P] [US3] Unit tests for restart backoff and the connection state machine in `internal/driver/supervisor_test.go`: five consecutive failures reach `faulted`, a device-set change resumes retries, `retrying` carries `next_retry_at`, no oscillation (R14 §7 and §8)
- [ ] T058 [P] [US3] Integration test in `internal/api/restart_test.go`: start with a stored configuration and confirm the selected coordinator and ready state return with zero API calls (SC-003, SC-006), and that `keys.in_effect` becomes true within 20 s p95 of a key change (plan.md §Performance Goals)
  - The SC-003/SC-006 half is done and passing (`TestRestartRestoresSelectionAndReady`, `TestRemovalAndReturnKeepsReady`). The `keys.in_effect` clause cannot be verified in this repository yet: there is no HTTP key API (`internal/api/keys.go`, the `keys` block of the network body) and no driver apply path that renders stored keys into the child's config and sets `provisioning.applied_revision`, so no request can change a revision and no report can say one is in effect. The test carries a comment naming the three pieces that must land first; T058 stays unticked until they do.
  - Re-checked on the 2026-11-15 Phase 07 re-run; the gap is exactly the unrun Phase 06 tasks. Phase 05's hub-side work is in place (`hub.SubmitKeys`, `hub.GenerateKeys`, `hub.Keys`, `DeriveReadiness` with `keys_not_in_effect`), so nothing here is unsolved, only unbuilt: T047 renders keys into `internal/driver/config.go` (`ConfigSpec` still carries only cache dir and log level), T048 is the only writer of `provisioning.applied_revision` (`store.SetAppliedRevision` has no caller outside its own test), T049 adds `internal/api/keys.go`, and T051 gives `buildNetworkStatus` real readiness — it writes `snap.KeyRevision`, computes neither `applied_revision` nor `in_effect`, and appends `keys_missing` for every selected coordinator. `go test ./internal/driver/... ./internal/api/...` passes as the tree stands; finish Phase 06, then re-run this document to land T058's second clause in the network body's `keys.in_effect`, bounded by the 20 s p95 of plan.md §Performance Goals.
- [x] T059 [US3] Implement boot-time restore in `internal/hub/coordinator.go`: load the provisioning record, match the discovered identity, start the driver only when the device is attached, report `awaiting_coordinator` otherwise (FR-015, FR-016)
- [x] T060 [US3] Implement automatic adoption on re-plug in `internal/hub/coordinator.go`: the same identity at a new path returns to ready; an unknown device stays available without changing the selection (FR-004, FR-016)
- [x] T061 [US3] Implement backoff and deadlines in `internal/driver/supervisor.go`: exponential backoff, 30 s start deadline, `faulted` after five failures with no device change, kill and record the last stderr line redacted (R14 §8)
- [x] T062 [US3] Implement debounce and change notification in `internal/coordinator/discover.go`: 300 ms debounce, one change set per settle, notifying the supervisor to resume (R14 §7)
- [x] T063 [US3] Implement disconnect and reconnect transitions in `internal/driver/state.go`: `ready → not_attached` on unplug, `not_attached → connecting → ready` on return, selection untouched throughout

## Completion

- [x] `go test ./internal/driver/... ./internal/api/...` passes, including `restart_test.go` (SC-003, SC-006) and the five-failure `faulted` transition.
- [x] US3 independent test: configure, restart the hub with no API calls, confirm ready; simulate removal and return of the device node in the fixture and confirm the same result.
- [x] Verify all changes work as expected
- [x] Run `/speckit.analyze` to verify consistency
  - Run read-only against `spec.md`, `plan.md`, `tasks.md` and `.specify/memory/constitution.md`. Zero CRITICAL findings; the report (in the session transcript) flags the T058 dependency on the US2 key API that blocked its second clause here, the `ZWAVE_HUB_DESTRUCTIVE_OPS` gating conflict (INC-4), and the stale checkboxes in `tasks.md`.
