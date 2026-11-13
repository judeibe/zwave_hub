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
- [ ] T021 [US1] Implement identity derivation in `internal/coordinator/identity.go`: `usb:<vid>:<pid>:<serial>` or `usb:<vid>:<pid>:port:<topology>`, `identity_kind`, duplicate detection, `selectable` rule
- [ ] T022 [US1] Implement the discovery poller in `internal/coordinator/discover.go`: 2 s poll of `/sys/class/tty`, `/dev` cross-check, `unix.Access` on the resolved path to separate `permission_denied` from `not_passed_through`, `/dev/serial/by-id/...` preference, 300 ms debounce, snapshot publish
- [ ] T023 [US1] Implement the coordinator store in `internal/store/coordinators.go`: upsert by id, `support` transition rules, `last_path`, `last_seen_at`, list and get, plus the singleton provisioning row read/write for `home_id`, `key_revision` and `applied_revision`
- [ ] T024 [US1] Implement selection and switching in `internal/hub/coordinator.go`: commit through the command queue, reject unsupported devices, accept an unverified device and detach it if identification later reports it unsupported, enforce the FR-003 `home_id` gate, retain the selection when the device disappears

## Completion

- [ ] `go test ./internal/coordinator/... ./internal/hub/... ./internal/store/...` passes.
- [ ] Identity tests cover serial, topology fallback, placeholder serials and `ambiguous_identity`; selection tests cover the unverified-selectable and detach-on-unsupported cases.
- [ ] The discovery poller runs against the sysfs fixture tree and reports a debounced snapshot.
- [ ] Verify all changes work as expected
- [ ] Run `/speckit.analyze` to verify consistency
