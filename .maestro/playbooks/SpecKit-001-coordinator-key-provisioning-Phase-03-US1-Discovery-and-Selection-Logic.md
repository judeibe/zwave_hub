# Phase 03: User Story 1 — Discovery, identity and selection (Priority: P1) 🎯 MVP

First half of the MVP: hardware identity derivation, sysfs enumeration, the discovery poller, the coordinator store and the selection rules, including the FR-003 network-identifier gate and the unverified-device handling. Tests are written first.

## Spec Kit Context

- **Spec Kit phase:** Phase 3a of tasks.md (this file is execution phase 03 of 09)
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

- T018, T019

## Tasks

- [x] T018 [P] [US1] Unit tests for hardware identity in `internal/coordinator/identity_test.go`: `usb:<vid>:<pid>:<serial>`, topology fallback, placeholder serials (`0001`, all zeros), `ambiguous_identity` for cloned serials (FR-004, R5, R14 §11)
- [x] T019 [P] [US1] Unit tests for selection and switching in `internal/hub/coordinator_test.go`: unsupported rejected, an unverified device accepted as selectable, a selection detached when identification later reports it unsupported, switch allowed with no secure nodes, same `home_id` accepted, different or absent `home_id` rejected (FR-003)
- [x] T020 [US1] Implement sysfs enumeration in `internal/coordinator/sysfs.go`: tty node → USB attributes through `/sys/class/tty`, reading `idVendor`, `idProduct`, `serial`, `manufacturer`, `product`
- [x] T021 [US1] Implement identity derivation in `internal/coordinator/identity.go`: `usb:<vid>:<pid>:<serial>` or `usb:<vid>:<pid>:port:<topology>`, `identity_kind`, duplicate detection, `selectable` rule
- [x] T022 [US1] Implement the discovery poller in `internal/coordinator/discover.go`: 2 s poll of `/sys/class/tty`, `/dev` cross-check, `unix.Access` on the resolved path to separate `permission_denied` from `not_passed_through`, `/dev/serial/by-id/...` preference, 300 ms debounce, snapshot publish
- [x] T023 [US1] Implement the coordinator store in `internal/store/coordinators.go`: upsert by id, `support` transition rules, `last_path`, `last_seen_at`, list and get, plus the singleton provisioning row read/write for `home_id`, `key_revision` and `applied_revision`
- [x] T024 [US1] Implement selection and switching in `internal/hub/coordinator.go`: commit through the command queue, reject unsupported devices, accept an unverified device and detach it if identification later reports it unsupported, enforce the FR-003 `home_id` gate, retain the selection when the device disappears

## Completion

- [x] `go test ./internal/coordinator/... ./internal/hub/... ./internal/store/...` passes.
- [x] Identity tests cover serial, topology fallback, placeholder serials and `ambiguous_identity`; selection tests cover the unverified-selectable and detach-on-unsupported cases.
- [x] The discovery poller runs against the sysfs fixture tree and reports a debounced snapshot.
- [x] Verify all changes work as expected
- [x] Run `/speckit.analyze` to verify consistency

## Execution Notes

Phase 03 ran in one iteration: T018–T024, sequential except the T018+T019 `[P]` batch. No images were attached to this playbook.

| Task | Commit | Evidence |
|---|---|---|
| T018, T019 | `9a43904` | Tests written against declaration skeletons, so both packages compiled and every test failed before the implementations landed. |
| T020 | `99e2f74` | `sysfs_test.go` runs `Enumerate` against `internal/coordinator/testdata/sys`. |
| T021 | `b3ca3db` | `identity_test.go`: serial, topology fallback, placeholder serials, `ambiguous_identity`, `Selectable`. |
| T022 | `00ae7e1` | `discover_test.go`: fixture `/dev` cross-check, `permission_denied`, debounce settle, failed-publish retry, `Run` publishing a debounced snapshot. |
| T023 | `edf681b` | `coordinators_test.go` against a real SQLite file, `-race`. |
| T024 | `9273107` | The six T019 tests plus a scratch test (written, run, deleted) for retain-on-disappear, the persisted detach and the persisted `home_id`. |

Verification: `go build ./...` clean; `go vet ./...` clean; `gofmt -l cmd internal` empty; `go test ./... -race -count=1` ok in config, coordinator, event, hub and store; `make lint` ok.

### /speckit.analyze — read-only consistency findings

Scope: `spec.md`, `plan.md`, `tasks.md`, `data-model.md` §2 and §9.2, `contracts/openapi.yaml`, `contracts/events.md`, `.specify/memory/constitution.md`. No files were modified by the analysis; the fixes below are recommendations for a later editing command.

| ID | Category | Severity | Location(s) | Finding | Recommendation |
|---|---|---|---|---|---|
| C1 | Inconsistency | HIGH | `contracts/openapi.yaml:534-548` vs `:615-624`; `contracts/events.md:118` | `ambiguous_identity` is a documented `support_reason` and `coordinator.rejected` code, and the hub returns it as a `409` `Problem.code`, but the `Problem.code` enum omits it. A client generated from the contract rejects the hub's own refusal body. | Add `ambiguous_identity` to the `Problem.code` enum. |
| C2 | Inconsistency | MEDIUM | `spec.md:137` (FR-003) vs `data-model.md` §9.2 row 2 | FR-003 gates a replacement on securely included devices alone; the data-model table adds "and no key revision". The implementation follows the data model (`secureNodeCount > 0 \|\| keyRevision > 0`). | Amend FR-003 to include the key-revision condition. |
| C3 | Underspecification | MEDIUM | `data-model.md` §9.2; `contracts/events.md:118` | A `PUT /active-coordinator` for an unknown id has no lifecycle row and `coordinator_not_found` is not a `coordinator.rejected` code, so the artifacts do not say whether the refusal is audited or emitted. The implementation audits it and emits nothing. | Add the not-found row to §9.2 and state that it is audited without an event, or add the code to events.md. |
| C4 | Ambiguity | LOW | `data-model.md` §2 (`support_reason`) | The column is defined as the durable verdict only; nothing says what the API reports while `support = unverified`, so `permission_denied`, `not_passed_through` and `ambiguous_identity` would be unreportable. The implementation falls back to the discovery cause while unverified. | State the fallback rule in §2. |
| C5 | Traceability | LOW | `tasks.md` (whole) | FR-001, FR-002, FR-007, FR-009, FR-010 and FR-020 are implemented by T020–T024 and T042–T052 but never cited, so a scan for requirement IDs under-reports coverage. | Cite the IDs in the relevant task text, or add a coverage table. |
| C6 | Progress record | LOW | `tasks.md:27-30` | Only T001–T004 carry `[x]`; T005–T024 are implemented and committed but unticked, so `tasks.md` is not a usable progress record across Auto Run phases. | Decide one owner for `tasks.md` checkboxes (the phase playbooks or `/speckit-implement`) and apply it consistently. |

Coverage: 21 FRs (FR-001–FR-021), all with implementing work; 15 of 21 cited in `tasks.md` (71%). 83 tasks (T001–T083). No duplicated requirements, no unmapped tasks, no unresolved placeholders. Constitution III and VI hold in the code this phase added: no `init()`, no environment read outside `internal/config` (the discovery roots are injected), package-level `var` is limited to immutable sentinel errors, and every event goes through `internal/event`. No CRITICAL finding, so nothing blocks the next phase.

## Manual Follow-Up (not executed by Auto Run)

- Run the discovery poller against a real coordinator in the Docker image on Linux, which the Platform Notes reserve for real-device checks; the fixture trees cannot show how a given USB serial adapter names its `by-id` link.

