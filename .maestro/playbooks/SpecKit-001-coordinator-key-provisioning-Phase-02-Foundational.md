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
- [x] T012 [P] Implement RFC 9457 problem responses and the error-code mapping in `internal/api/problems.go`
- [x] T013 Implement middleware in `internal/api/middleware.go`: request id, actor capture (remote address plus optional self-declared label, recorded unverified), panic recovery, one access event per request
- [x] T014 Implement the router in `internal/api/router.go`: method-and-wildcard `ServeMux` under `/api/v1` plus `getHealth` at `/healthz` and `getReady` at `/readyz`, JSON content type, one mount point per resource for later phases
- [x] T015 [P] Create `internal/api/presenters.go` as the single constructor of response bodies, with the redaction helper that refuses any field on the never-send list (FR-013, SC-004)
- [x] T016 [P] Write store tests in `internal/store/store_test.go`: migrate from empty and from `user_version` 0, foreign keys enforced, integrity check, crypto round-trip and tamper detection
- [x] T017 [P] Write command-queue tests in `internal/hub/commands_test.go`: arrival-order serialization under concurrent submissions, commit plus audit atomically, later-wins per class, no intermediate snapshot (FR-021, SC-009)

## Completion

- [x] `go test ./internal/store/... ./internal/hub/...` passes, including the arrival-order and atomic commit-plus-audit tests.
- [x] `go vet ./...` is clean and `internal/config` is the only package that reads the environment (`grep -rn "os.Getenv" internal cmd` shows no other hit).
- [x] The router serves `/healthz` and `/readyz`; start the binary against a temp data directory and curl both.
- [x] Verify all changes work as expected
- [x] Run `/speckit.analyze` to verify consistency

---

## Execution Notes (run 1, 2026-11-13)

**Toolchain.** `go version go1.27.4 darwin/arm64`; nothing was installed this run. `modernc.org/sqlite` is imported from T008, so its `// indirect` marker is gone; `coder/websocket` and `x/sys` stay indirect until their phases. `go mod tidy` was still not run.

**Decisions taken where the task text left a choice.**

- **`config.Load(args []string, env Env, out io.Writer)`** takes the environment as a map so tests never touch the process; `config.Environ()` is the only `os.Environ` call in the repository. `ZWAVE_HUB_MASTER_KEY` deliberately has no flag: command lines are visible in `ps` and `docker inspect`.
- **The `Event` interface is closed** with an unexported `attrs()` method, so only `internal/event` can define an event and a handler cannot invent one. The emitter redacts any string carrying a run of 32 or more hex characters and drops `driver.log` lines that look like a secret.
- **One SQLite connection** (`SetMaxOpenConns(1)`) with the pragmas in the DSN, because a pool-opened connection can otherwise miss them. `PRAGMA integrity_check` runs before migrations, and a database whose `user_version` is newer than the binary is refused rather than migrated; migration numbers may have gaps.
- **`store.Tx` wraps `*sql.Tx`**, so `internal/hub` never imports `database/sql`: `WithTx(ctx, func(*Tx) error)` and `Tx.AppendAudit` are the seam, and `AppendAuditTx` was removed when T011 showed the hub would otherwise import SQL. Code inside `WithTx` may use only the `Tx` it is handed.
- **`hub.Worker[S]` is generic over the snapshot type**, because the real snapshot arrives with the coordinator phases. A failed command returns the zero snapshot and publishes nothing; the snapshot is published only after the commit. Queue capacity 16, 35 s command timeout, `ErrQueueFull` for the API to map to `503 command_queue_full` with `Retry-After: 1`.
- **The audit action set is the eleven of data-model.md §7**, each bound to exactly one outcome (`keys.rejected` and `network.reset_rejected` are not in §7; T066 and T080 own them). `detail` is a struct allow-list, never a map.
- **Request ids are generated in dashed UUID form**, so the 32-hex redaction rule cannot eat them; a client `X-Request-Id` that itself carries a 32+ hex run is replaced.
- **Unmatched routes are answered by the router's own handler** as problem+json with `invalid_request` and route `unmatched` (405 with `Allow` when the path exists under another method), so every response is JSON and every request still gets a request id and one access event.
- **`Present` is the only body constructor.** It refuses never-send member names, `[]byte` (base64), and 32+ hex strings; `KeyValueBody` exempts only the hex rule, and no type implements it yet (T052's confirmed export is the intended one). `WriteProblem` and `writeJSON` route through it, so `encoding/json` is imported for exactly one call.
- **`main.go` is wired only as far as this phase needs**: config, emitter, store, router, `http.Server` and signal handling. Readiness asks the store; the command worker, discovery, the supervisor and PID 1 are T034's, and T034 must extend the readiness predicate to include the running worker (contracts/openapi.yaml `/readyz`).

**Bugs the tests found, fixed in the same commits.** T016 found that a hub.db whose header is unreadable fails at `PingContext` before `integrity_check` runs, so `store.integrity_failed` was never emitted for the most likely real corruption; `store.go` now emits it on a ping failure too. T017 found that a failed command returned the snapshot its `Apply` had built even though the transaction rolled back; `commands.go` now returns the zero snapshot on any error. T015's integration pass found `problems.go` still calling `json.Marshal`; it now calls `Present`, and the T012 probe file `internal/api/scratch_test.go` was deleted.

**Verified by running it.** `gofmt -l cmd internal`, `go build ./...`, `go vet ./...`, `go test ./internal/... -race -count=1`, `go test ./internal/store/... ./internal/hub/...` (the completion command), `make test`, `make lint` (which is gofmt, vet and `TestNoKeyMaterial`), and `grep -rn "os.Getenv" internal cmd` (no hits; the only environment read is `os.Environ` in `internal/config`). The binary was started against a fresh temp data directory and `--bind 127.0.0.1:18099` and answered `GET /healthz` with `200 {"status":"ok"}`, `GET /readyz` with `200 {"status":"ready"}`, and `GET /nope` with `404` problem+json and route `unmatched`; the directory held `hub.db` and no `master.key` (that file is created on first key use, not at open), the log held `store.migrated` then one `api.request` per request, and `SIGTERM` exited 0.

**Known limits recorded, not fixed here.** `Present` inspects values, not custom `MarshalJSON` output, and it walks `[N]byte` arrays rather than refusing them (bodies carry `[16]byte` identifiers that json renders as numbers); a body using either to hold a key would bypass the refusal, and the file says so where a reviewer will read it. The single connection means a handler read waits behind a worker transaction, so every store read must stay short. `hasHexRun` now exists in both `internal/api/middleware.go` and `internal/event/event.go`; the two copies are identical today and should be hoisted into one package when a third caller appears. `internal/api` still has no committed test file, so `Present`'s refusal rules are proven only by the T015 probe; T035 is where API tests land.

**`/speckit.analyze` (read-only), run after the batch.** Cross-checks that passed: the event catalogue matches events.md §3 name for name (32/32, and the three names that looked missing — `keys.generated`, `keys.replaced`, `network.reset` — are audit actions in §4, not events); the problem codes match `Problem.code` in openapi.yaml exactly (16/16, same names); the audit actions match data-model.md §7 (11/11 with the stated outcomes); `001_init.sql` matches §3–§7 field for field; and the `/healthz` and `/readyz` bodies match the contract. Open findings:

- No audit action exists for a refused key submission or a refused reset, though FR-014 and FR-019 require both to be audited. Carried from Phase 01; T066 and T080 own it.
- `plan.md:43` gates reset **and export** behind `ZWAVE_HUB_DESTRUCTIVE_OPS`; the contract and the env example gate reset only. T080.
- `research.md` R12 names `make test-e2e`, which no task creates and the Makefile does not define — a constitution X gap with no owner. Carried.
- `node:26-slim` in T072 versus `node:26.13.1-bookworm-slim` in research.md R11. Carried.
- data-model.md:53 points `support_reason` at "the reason codes in §7", which is the audit section; the six FR-006 codes are enumerated in no design document. Carried.
- `keys.apply_failed` is in the audit enum and §7 but has no row in the events.md §4 pairing table. Carried.
- `specs/001-coordinator-key-provisioning/tasks.md` shows T001–T004 ticked and T005–T017 unticked; this playbook is the execution record, so whoever maintains tasks.md should reconcile it rather than treat it as status.
- `/readyz` is specified as ready only when "the command worker is running", but this phase has no worker; `main.go` checks the store alone. T034 must widen the predicate, and the note is repeated in the decision list above so it is not lost.
