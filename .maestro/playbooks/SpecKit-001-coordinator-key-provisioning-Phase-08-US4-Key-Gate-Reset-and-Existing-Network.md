# Phase 08: User Story 4 — Replace keys or reset the network deliberately (Priority: P3)

Adds the in-use key gate that refuses replacement while secure devices exist and names the reset path, the confirmed network reset with its observable post-conditions, and reporting of an unknown pre-existing network.

## Spec Kit Context

- **Spec Kit phase:** Phase 6 of tasks.md (this file is execution phase 08 of 09)
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

## Default Decisions Applied

The last `/speckit.analyze` left four findings open. These documents apply the defaults below so execution needs no input. Edit this section before running if you want a different answer.

- **Rejected-work audit actions (INC-11):** refused key work is audited. Use the action names `keys.rejected` (in-use refusal or validation failure, with a `reason` and `classes` in `detail`) and `network.reset_rejected` (reset presented with a bad, expired or consumed token). Add both to the `AuditAction` enum in `contracts/openapi.yaml`, to `data-model.md` §7, and to the `contracts/events.md` §4 pairing table **before** writing the audit code in T053/T066. Pair `keys.rejected` with the `keys.in_use_rejected` event.
- **Unverified coordinator detached after identification (INC-14):** when identification reports `unsupported`, clear `active_coordinator_id`, set the reason to `not_a_zwave_controller`, and write a `coordinator.rejected` audit row. Add the transition to `data-model.md` §9.2.
- **Queue-full refusals (INC-13):** a `503 command_queue_full` rejection happens before queue admission and writes no audit row; this is the one carve-out from "every refusal is audited".
- **Export gating (INC-4):** `ZWAVE_HUB_DESTRUCTIVE_OPS` gates `resetNetwork` only, as the contract, env example and T070 already do. Leave `plan.md:43` as a known documentation discrepancy for T080 to reconcile.

## Platform Notes

- Development host is macOS arm64; production target is Linux. Sysfs discovery is Linux-specific, so make the sysfs root an injected path and test against a fixture tree under `testdata/`. Real-device checks run only in the Docker image on Linux.
- The Go toolchain was not installed when these documents were generated. Phase 01 verifies and installs it.

## Parallel Batches

Delegate each task in a batch to its own sub-agent, concurrently:

- T064, T065

## Tasks

- [x] T064 [P] [US4] Unit tests for the in-use gate in `internal/hub/keys_test.go`: per-class refusal while secure nodes exist, reset named in the reason, nothing written (FR-014) — Done (commit 236b9b1): `TestInUse*` cover per-class refusal, reset named in the error, nothing written; verified passing.
- [x] T065 [P] [US4] Unit tests for the reset transition in `internal/hub/reset_test.go`: keys cleared, `home_id` NULL, `key_revision = 0`, `applied_revision` NULL, selection retained (FR-017, data-model.md §9.1) — Done (commit 236b9b1): `TestReset*` cover keys cleared, `home_id`/revisions cleared, selection retained; verified passing.
- [x] T066 [US4] Implement the in-use gate in `internal/hub/keys.go`: refuse replacement or regeneration for a class protecting included devices, name the reset path, audit the rejected outcome (FR-014, FR-021) — Done (commit acb8d92): `refuseInUse` gate with `keys.rejected` audit row and `keys.in_use_rejected` event.
- [x] T067 [US4] Add `hard_reset` command support in `internal/driver/client.go`: send, never wait for a forwarded completion, expose the polled state that `internal/hub/reset.go` waits on (FR-017) — Done: `Client.HardReset` sends `driver.hard_reset` without awaiting completion; `Client.ControllerState` is the polled state.
- [x] T068 [US4] Implement reset orchestration in `internal/hub/reset.go`: one command that clears keys, `home_id` and revisions, calls the client's `hard_reset`, confirms completion by polling `controller.get_state`, audits `network.reset` or `network.reset_failed` (FR-017) — Done (commit 2744c63): `Hub.Reset` clears keys/home_id/revisions, polls the controller, audits `network.reset` or `network.reset_failed`.
- [x] T069 [US4] Implement unknown-network reporting in `internal/hub/readiness.go`: `existing_network_detected` with `home_id` and `node_count`, generation blocked, the two permitted paths reported (FR-018, R14 §10) — Done (commit 13cd8ed): `existing_network_detected` blocking reason with `home_id`, `node_count` and both permitted paths.
- [x] T070 [US4] Implement the reset endpoint and confirmation in `internal/api/confirmations.go`: `resetNetwork` in two steps, and `503 destructive_ops_disabled` when `ZWAVE_HUB_DESTRUCTIVE_OPS=disabled` — Done: `resetNetwork` two-step endpoint and `503 destructive_ops_disabled` already live in `internal/api/confirmations.go` and wired in `cmd/zwave-hub/main.go`; now covered by T071's tests.
- [x] T071 [P] [US4] Integration test in `internal/api/reset_test.go`: refusal with secure nodes present, confirmed reset to unprovisioned, readiness false afterwards (quickstart scenario 10) — Done: `internal/api/reset_test.go` adds three tests (refusal with secure nodes, two-step reset to unprovisioned with readiness false and single-use token, destructive-ops-disabled). Mutation-checked: removing `DeleteAllKeys` from the reset makes the test fail.

## Completion

- [x] `go test ./...` passes, including `keys_test.go`, `reset_test.go` (both files) and the reset integration test. — Verified: `go test ./...`, `go test -race ./internal/hub ./internal/api` and `go test -tags contract ./tests/contract/...` all pass; `gofmt` and `go vet` clean.
- [x] US4 independent test: with a secure node present, key replacement is refused with `409 key_in_use` and an audit row; a confirmed reset leaves no keys, no `home_id`, `key_revision` 0, the coordinator still selected, and readiness false. — Verified by `TestResetRefusesKeyChangeWhileSecureNodesExist` and `TestResetNeedsTheTwoStepConfirmationAndLeavesUnprovisioned`.
- [x] Refusals are audited with outcome `rejected` (FR-014, FR-019). — Verified: two `keys.rejected` rows with outcome `rejected` after two refusals; bad or replayed reset tokens write `network.reset_rejected`.
- [x] Verify all changes work as expected — Verified, see the three items above.
- [x] Run `/speckit.analyze` to verify consistency — Run, read-only, scoped to US4: no gaps. FR-014, FR-017, FR-018, FR-019 and FR-021 each map to implemented, tested code (T064–T071), and `keys.rejected` and `network.reset_rejected` appear in `openapi.yaml`, `data-model.md` §7 and `events.md` §4. Only known carry-over: INC-4, the `plan.md:43` export-gating wording, which Phase 09 T080 reconciles.
