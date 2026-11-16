# Phase 06: User Story 2 — Key application, handlers, export and redaction (Priority: P1)

Second half of the feature request: rendering keys into the sidecar config, applying them by restarting only the child, the key handlers, the two-step confirmed export, readiness in the network status, and the redaction tests. At the end the network reports ready for secure inclusion and keys survive a restart.

## Spec Kit Context

- **Spec Kit phase:** Phase 4b of tasks.md (this file is execution phase 06 of 09)
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

- T054, T055, T056

## Tasks

- [x] T047 [US2] Extend `internal/driver/config.go` with key rendering: `securityKeys` and `securityKeysLongRange` as `Buffer.from("<hex>","hex")`, omit absent classes rather than writing empty entries, same property names in both sets. Done: `ConfigSpec.Keys`/`LongRange` (raw bytes), fixed property order, `HasKeys()`; tests invert the old "no securityKeys" assertion and cover omission.
- [ ] T048 [US2] Implement key application in `internal/driver/supervisor.go`: re-render the config from the database, restart only the child, set `applied_revision` when `start_listening` succeeds, persist the controller's `home_id` to `provisioning.home_id` on that same path, emit `keys.apply_failed` when it does not (FR-012, FR-015)
- [ ] T049 [US2] Implement key handlers in `internal/api/keys.go`: `getKeyConfiguration`, `submitKeys`, `generateKeys`
- [ ] T050 [US2] Implement the two-step export in `internal/api/confirmations.go`: `exportKeys` returns a token and no values first, then values with the token; unknown and wrong-actor tokens are `404 confirmation_invalid`; expired and consumed tokens are `410 confirmation_expired`, matching contracts/openapi.yaml:333; audit `keys.export_requested|exported|export_rejected`
- [ ] T051 [US2] Extend `internal/api/network.go` with readiness, blocking reasons and the `keys.in_effect` block
- [ ] T052 [US2] Add key response shapes and the export-only value path to `internal/api/presenters.go`, keeping every non-export body metadata-only
- [ ] T053 [US2] Register key audit actions in `internal/store/audit.go`, including a rejected-submission action so refused key work is attributable (FR-019, FR-021, SC-009)
- [ ] T054 [P] [US2] Integration test in `internal/api/keys_test.go`: submit, generate, invalid submission, single-class replacement, no coordinator attached, metadata-only reads (quickstart scenarios 4–7)
- [ ] T055 [P] [US2] Fixture test in `tests/contract/readiness_test.go`, with payloads under `tests/contract/fixtures/`: recorded `start_listening` payloads for the secure-node states the mock controller cannot produce
- [ ] T056 [P] [US2] Redaction test in `internal/api/redaction_test.go`: no key hex appears in any response, error, log record or audit row across all operations (SC-004)

## Completion

- [ ] `go test ./...` and `make test-contract` pass, including `internal/api/redaction_test.go` (SC-004).
- [ ] US2 independent test: from a selected mock coordinator with no keys, observe automatic generation of the four classic keys (zero-node guard), then `GET /api/v1/network` reports `ready_for_secure_inclusion: true`; restart the hub and confirm the same.
- [ ] Export: first call returns a token and no values; second call with the token returns values and writes an audit row without them.
- [ ] Verify all changes work as expected
- [ ] Run `/speckit.analyze` to verify consistency
