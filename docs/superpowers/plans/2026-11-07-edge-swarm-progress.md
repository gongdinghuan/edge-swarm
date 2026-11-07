# Execution ledger — plan: 2026-11-07-edge-swarm-mvp.md

- Ruling: This workspace has no git history or existing code. Initialize a feature branch in place; there is no current checkout to isolate.
- Ruling: SQLite is an explicit development mode; PostgreSQL remains the production database. This enables runnable local verification without Docker. Production SQL still needs real PostgreSQL integration validation.
- Ruling: Implement a complete P1/P2 vertical slice, not pretend the 8–12 week design is finished. SQL polling provides durable dispatch; NATS optimization and multi-relay HA remain later milestones.
- Pre-flight: store Task 1 supplies transactional authority to Task 3; backend Task 2 supplies context-aware Generate to Agent Task 3; binaries Task 4 wire both together. No conflicting interfaces.
- Execution: native implementation with test-first cycles; final independent review required by executing-plans skill.

## Implemented and verified so far

- Task 1: durable SQL authority implemented. Tests first failed for missing Store/Open, then passed: tenant idempotency, conflicting payload, concurrent exclusive claim, expiry, reconnect fencing, revocation, durable cancellation and reopen. Queue bound added RED→GREEN for ErrCapacity. PostgreSQL-specific test exists but is skipped without a dedicated DSN.
- Task 2: compatible text backend implemented. Tests first failed for missing New, then passed request preservation, SSE output, HTTP error propagation, cancellation and missing-DONE failure.
- Task 3: control/Agent network slice implemented. Tests first failed for missing Agent/control packages, then passed real outbound WebSocket plus actual HTTP model fixture, chat, async job, tenant isolation, cancellation and interrupted stream semantics.
- Task 4: control/Agent/simulator binaries built, local executable smoke passed. Simulator clearly marked SIMULATED; all processes started by smoke were stopped. Deployment files supplied but not tested in Docker.
- Verification before final review: 12 tests pass, 1 PostgreSQL test explicitly skipped; `go vet ./...` passes; `go test -race ./...` passes. Full executable smoke chat and batch both succeed.

## Findings and environment decisions

- The default Go module proxy was unreachable; dependency downloads succeeded using GOPROXY=https://goproxy.cn with Go checksum verification still enabled. go.mod/go.sum lock the resolved dependencies; toolchain archive SHA-256 matched official release metadata.
- Cancellation fixture initially hung because its HTTP handler waited on context without consuming the request body. Consuming that body made the test exercise real client disconnection correctly; production cancellation logic was not bypassed.
- Ruling: No existing base commit or remote exists; keep new implementation locally without publishing or merging. Git repository initialized on feat/edge-swarm; do not create unrelated base history merely to fit a branch workflow.
- Real GPU, public three-site testing, PostgreSQL server, Docker deployment and DeepInfer D1–D3 remain unverified. The README lists implemented scope and remaining design stages explicitly.

## Independent review and fixes

- Final: fixed output persistence failure silently discarding a delta then completing successfully — TestOutputPersistenceFailureCannotBecomeSuccessfulStream reproduced the truncated successful stream using a real SQLite failure trigger (RED), then passed after poisoning the connection and failing execution (GREEN).
- Final: fixed slow models responses retaining the global scheduling mutex — TestSlowModelsClientDoesNotHoldGlobalLock RED→GREEN; model snapshot and task-create outcomes now write responses outside the lock.
- Final: fixed slow SSE writers surviving request deadline — TestSlowStreamWriteStopsAtRequestDeadline RED→GREEN using a deterministic deadline-aware stalled transport fixture; content, success and error frames now use bounded write deadlines. A real slow public-network socket test remains part of deployment acceptance.
- Independent reviewer reported no Critical issues; all 3 Important findings were reproduced and fixed. No deferred minor findings were reported. No post-fix re-review dispatch; regression tests and full/race suites verify the fix pass.

## Final verification

- `go test ./... -count=1 -timeout 60s`: 15 tests passed, 1 real PostgreSQL test skipped, 0 failures (machine-readable report in ignored data/test-report.jsonl).
- `go test -race ./... -count=1 -timeout 90s`: passed on windows/amd64 with the available GCC toolchain; PostgreSQL test still skipped.
- `go vet ./...`: passed. `go build -o bin/ ./cmd/...`: passed, produces control.exe, agent.exe and simulator.exe.
- Final `scripts/smoke.ps1`: passed actual executable chat and async task through HTTP/WSS-compatible loopback WebSocket transport, using explicitly simulated model output; script stops its processes. This is local transport verification, not TLS/public-network/GPU certification.
- All tasks of this incremental plan are complete. Full system milestones outside this plan remain listed in README. Files retained locally on feat/edge-swarm; no remote push, merge or deployment was performed.
