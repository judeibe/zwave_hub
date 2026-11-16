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
- [x] T058 [P] [US3] Integration test in `internal/api/restart_test.go`: start with a stored configuration and confirm the selected coordinator and ready state return with zero API calls (SC-003, SC-006), and that `keys.in_effect` becomes true within 20 s p95 of a key change (plan.md §Performance Goals)
  - **First clause (SC-003, SC-006) is done and verified.** `TestRestartRestoresSelectionAndReady` and `TestRemovalAndReturnKeepsReady` both PASS on this tree (`go test ./internal/api/ -run 'TestRestartRestoresSelectionAndReady|TestRemovalAndReturnKeepsReady' -count=1 -v`). A stored selection and home ID come back from the database with only GETs in the second lifetime, the `coordinator.selected` audit count stays at one across both lifetimes, and removal plus return of the same identity repeats ready → awaiting_coordinator → ready without a second selection.
  - **Second clause (`keys.in_effect` within 20 s p95) is done and verified (2026-11-16).** Phase 06 landed on this tree (T047–T056), which is what had blocked it: `internal/api/keys.go` serves `PUT /api/v1/keys`, `driver.RenderConfig` renders the stored keys, `hub.NoteKeysApplied` reaches `store.SetAppliedRevision`, and `buildNetworkStatus` fills `Keys.AppliedRevision`, `Keys.InEffect` and `Keys.ClassicComplete`. The clause is the block appended to `TestRestartRestoresSelectionAndReady` after the re-plug section: submit a fresh classic set through `PUT /api/v1/keys`, assert the submission response and the network body both say `keys.in_effect: false`, have the fixture make the driver's report through `hub.NoteKeysApplied` (the call the bridge makes after `start_listening`, standing in exactly as `Adopt` stands in for the ready report), then poll `GET /api/v1/network` through `awaitKeysInEffect` until `keys.in_effect` is true with `keys.applied_revision == keys.revision`, failing past `keysInEffectBudget` (20 s, plan.md §Performance Goals).
  - **The bound was proved to bite, not merely to pass.** With the `NoteKeysApplied` call removed the test fails at 20.02 s with `keys.in_effect still false ... want true within the 20s budget`; restored, it passes in 0.02 s. Full suite `go test ./... -count=1` passes, and `gofmt -l` / `go vet ./...` are clean.
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
