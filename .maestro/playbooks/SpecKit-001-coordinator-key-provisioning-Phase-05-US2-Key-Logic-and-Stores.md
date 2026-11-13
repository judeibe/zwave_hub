# Phase 05: User Story 2 — Key validation, generation, stores and readiness (Priority: P1)

First half of the feature request itself: the pure key logic (validation, generation, readiness), the encrypted key store, the confirmation-token store and the key domain operations, tests first. Apply the audit-action decision from Default Decisions before touching audit code.

## Spec Kit Context

- **Spec Kit phase:** Phase 4a of tasks.md (this file is execution phase 05 of 09)
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

Delegate each task in a batch to its own sub-agent, concurrently. Batches run in listed order; wait for a batch to finish before starting the next.

- T037, T038, T039, T040 — tests first. Run them and confirm they fail before starting the next batch.
- T041, T042

## Tasks

- [x] T037 [P] [US2] Unit tests for key validation in `internal/hub/validate_test.go`: 32 hex characters / 16 bytes, atomic rejection naming the offending class, intra-set duplicate rejected, cross-set duplicate warned (FR-011, R14 §9)
- [x] T038 [P] [US2] Unit tests for readiness in `internal/hub/readiness_test.go`: the predicate and all eight blocking reasons in the order of data-model.md §8.3, each carrying its fields (SC-002)
- [x] T039 [P] [US2] Unit tests for confirmation tokens in `internal/store/confirmations_test.go`: hash-only storage, 120 s expiry, single use, operation- and actor-bound, prune older than 24 h
- [x] T040 [P] [US2] Unit tests for key generation in `internal/hub/keygen_test.go`: classic set only, zero-node guard, all-zero redraw, Long Range never generated automatically (R15)
- [x] T041 [P] [US2] Implement key validation in `internal/hub/validate.go`: 16-byte, 32-hex format for every class (FR-011), intra-set uniqueness, whole-submission rejection
- [x] T042 [P] [US2] Implement key generation in `internal/hub/keygen.go`: `crypto/rand` 16-byte values, all-zero and all-`0xFF` redraw, classic and Long Range sets kept separate
- [x] T043 [US2] Implement the key store in `internal/store/keys.go`: sealed rows, `key_revision` bump in the same transaction, `applied_revision` reads, decryption failure reported by class name and never repaired
- [x] T044 [P] [US2] Implement the confirmation store in `internal/store/confirmations.go`: SHA-256 token hash as key, operation and actor binding, `issued_at + 120 s` expiry, consume once, hourly prune
- [x] T045 [US2] Implement key domain operations in `internal/hub/keys.go`: submit a full set or one class, generate for selected classes, stored-versus-in-effect reporting, acceptance while the coordinator is faulted or absent (R14 §1 and §3), corrupt-store refusal with the three recovery paths, no in-use gate yet (that is US4), and the first-activation trigger: when a coordinator reaches `ready` with `key_revision == 0` and the controller reports zero nodes, generate the four classic keys and audit `keys.generated` before readiness may report true (FR-008, R15)
- [x] T046 [US2] Implement readiness derivation in `internal/hub/readiness.go`: the four-class predicate and the ordered blocking-reason builder from data-model.md §8.3

## Completion

- [x] `go test ./internal/hub/... ./internal/store/...` passes: validation, readiness (all eight blocking reasons in order), confirmation tokens and key generation. _Verified with `go test -count=1 -race`; `go vet` and `gofmt -l` clean._
- [x] No key value appears in any log record or error produced by these packages. _Grepped every `fmt.Errorf`/logger call in `internal/hub` and `internal/store` key files: errors cite class names only; no logger is used._
- [x] Verify all changes work as expected _`go build ./...`, vet, race tests all pass._
- [x] Run `/speckit.analyze` to verify consistency _Scoped read-only pass: T037-T046 map to FR-008/FR-011/R14/R15/SC-002 and are implemented. Open item (LOW, expected): `keys.rejected` and `network.reset_rejected` are not yet in `openapi.yaml`, `data-model.md` §7 or `events.md` §4; the Default Decisions place that edit before T053/T066 (a later phase). `tasks.md` checkboxes for T037-T046 are still unticked and can be synced in Phase 09._
