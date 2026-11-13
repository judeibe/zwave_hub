# Phase 02: Foundational (Blocking Prerequisites)

Builds configuration, structured events, the SQLite schema and store, key encryption, the append-only audit writer, the serialized command queue and the HTTP boundary. Every user story writes through these, so no story work may begin until their tests pass.

## Spec Kit Context

- **Spec Kit phase:** Phase 2 of tasks.md (this file is execution phase 02 of 09)
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

- T006, T007
- T009, T010
- T015, T016, T017

## Tasks

- [x] T005 Implement configuration in `internal/config/config.go`: flags plus the eight `ZWAVE_HUB_*` variables from quickstart.md §2, base64 32-byte master-key validation, discovery-interval floor of 500 ms, `destructive_ops` parsing, fail-fast errors, and no environment reads outside this package (constitution III)
- [x] T006 [P] Implement the emitter and event catalogue in `internal/event/event.go`: `log/slog` JSON handler only, typed events from `contracts/events.md`, allow-listed fields, and the never-send list enforced at construction (constitution VI)
- [x] T007 [P] Write the initial schema in `internal/store/migrations/001_init.sql` exactly as data-model.md §10, including `PRAGMA user_version = 1`
- [x] T008 Implement store open, pragmas and migration runner in `internal/store/store.go`: WAL, `synchronous=FULL`, `foreign_keys=ON` on every connection, `user_version` stepping, startup `PRAGMA integrity_check`
- [x] T009 [P] Implement AES-256-GCM seal/open and master-key loading in `internal/store/crypto.go`: `ZWAVE_HUB_MASTER_KEY` else `<data>/master.key` created `0600`, 12-byte nonce, AAD `zwave-hub/key/v1|<class>`, blob `[0x01][nonce][ciphertext‖tag]`
- [x] T010 [P] Implement the append-only audit writer and cursor pagination in `internal/store/audit.go`: one insert function, no update or delete path, action constants, `detail` allow-list from data-model.md §7
- [x] T011 Implement the serialized command worker in `internal/hub/commands.go`: one goroutine over a FIFO channel of capacity 16, each command one SQLite transaction that also writes its audit row, snapshot published only after commit; a full queue returns `503 command_queue_full` with `Retry-After: 1` and writes nothing (R9, FR-021, SC-009)
- [ ] T012 [P] Implement RFC 9457 problem responses and the error-code mapping in `internal/api/problems.go`
- [ ] T013 Implement middleware in `internal/api/middleware.go`: request id, actor capture (remote address plus optional self-declared label, recorded unverified), panic recovery, one access event per request
- [ ] T014 Implement the router in `internal/api/router.go`: method-and-wildcard `ServeMux` under `/api/v1` plus `getHealth` at `/healthz` and `getReady` at `/readyz`, JSON content type, one mount point per resource for later phases
- [ ] T015 [P] Create `internal/api/presenters.go` as the single constructor of response bodies, with the redaction helper that refuses any field on the never-send list (FR-013, SC-004)
- [ ] T016 [P] Write store tests in `internal/store/store_test.go`: migrate from empty and from `user_version` 0, foreign keys enforced, integrity check, crypto round-trip and tamper detection
- [ ] T017 [P] Write command-queue tests in `internal/hub/commands_test.go`: arrival-order serialization under concurrent submissions, commit plus audit atomically, later-wins per class, no intermediate snapshot (FR-021, SC-009)

## Completion

- [ ] `go test ./internal/store/... ./internal/hub/...` passes, including the arrival-order and atomic commit-plus-audit tests.
- [ ] `go vet ./...` is clean and `internal/config` is the only package that reads the environment (`grep -rn "os.Getenv" internal cmd` shows no other hit).
- [ ] The router serves `/healthz` and `/readyz`; start the binary against a temp data directory and curl both.
- [ ] Verify all changes work as expected
- [ ] Run `/speckit.analyze` to verify consistency
