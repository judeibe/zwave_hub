# Phase 01: Setup

Creates the Go module, the package skeleton, the pinned radio sidecar and the Makefile command surface that every later phase builds on. Nothing in the hub can compile or be tested until this exists, so this phase also verifies the Go and Node toolchains.

## Spec Kit Context

- **Spec Kit phase:** Phase 1 of tasks.md (this file is execution phase 01 of 09)
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

- T002, T003, T004

## Tasks

- [x] T001 Create the Go module and package skeleton in `go.mod`, `cmd/zwave-hub/main.go` and the `internal/{api,hub,coordinator,driver,store,event,config}` directories from plan.md §Source Code, with `go 1.27`
- [ ] T002 [P] Add runtime dependencies to `go.mod`: `modernc.org/sqlite v1.62.0`, `github.com/coder/websocket v1.8.15`, `golang.org/x/sys`
- [ ] T003 [P] Pin the radio sidecar in `sidecar/package.json` as `@zwave-js/server` 3.12.0 with `engines.node >= 20`, and commit the lockfile
- [ ] T004 [P] Add `Makefile` targets `build`, `test`, `test-contract`, `lint`, `run-mock`, `image`, `up`, `rollback-check`; each target is the only way to run its action (constitution X)

## Completion

- [ ] `go version` reports 1.27.x (install with `brew install go`, then let `GOTOOLCHAIN=go1.27.4` fetch the exact toolchain, if it is missing); record the result in the commit message.
- [ ] `go build ./...` succeeds on the empty skeleton and `make build` runs it.
- [ ] `make` lists every target named in T004 and each one is runnable (targets that need later code may print a clear not-yet message but must exist).
- [ ] Verify all changes work as expected
- [ ] Run `/speckit.analyze` to verify consistency
