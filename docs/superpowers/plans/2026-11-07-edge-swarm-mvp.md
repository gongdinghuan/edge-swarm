# Edge Swarm MVP Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans to implement task-by-task, with test-driven-development and one final independent code review.

**Goal:** Deliver runnable control and outbound Agent binaries with real OpenAI-compatible backend execution, durable tasks, lease fencing and authenticated streaming APIs.

**Architecture:** Go control service owns SQL state. Agents connect using outbound WebSocket and execute local compatible model endpoints. SQLite is a single-process development mode; PostgreSQL is the production path. Poll SQL for work in this increment; the later NATS outbox optimization does not change authority.

**Tech Stack:** Go, coder/websocket, database/sql, pgx, modernc SQLite, net/http.

**Spec:** ../specs/2026-11-07-edge-swarm-design.md

## Global Constraints

- Cross-public-network model sharding is outside this increment.
- No automatic retry after output_started; commit that marker before first content.
- Lease 30 seconds, renew every 10 seconds; reject stale fencing tokens and stale connections.
- Actual DeepInfer hardware integration remains gated D0–D3; do not label an HTTP-compatible backend as native DeepInfer.
- First increment uses one control/relay process; multi-relay ownership, HA, NATS, model downloads and automatic placement follow in later stages.
- Local dev uses explicit shared enrollment secret, hashed one-time enrollment tickets and individual revocable node secrets; TLS termination required for public deployment.

## Review Focus

- Stale workers must never overwrite results after lease expiry or reconnect.
- Duplicate API requests must not launch duplicate jobs, even concurrently.
- Tenant isolation covers lookup, cancellation and idempotency.
- Interrupted token streams must terminate with error and never show successful completion.
- Cancellation and bounded transport queues must not leak goroutines or leave capacity occupied.

## Files and interfaces

- cmd/control/main.go, cmd/agent/main.go: flags/env, graceful shutdown.
- internal/store/{store.go,store_test.go}: Open(driver,dsn), migrations, nodes, tasks, attempts, leases, output gating and result commit.
- internal/protocol/protocol.go: versioned wire messages and model request payload.
- internal/backend/{openai.go,openai_test.go}: streaming Generate(ctx,request,emit), authorization, timeout, malformed SSE handling.
- internal/control/{server.go,server_test.go}: authenticated APIs, enrollment/revocation, WebSocket sessions, dispatch and SSE.
- internal/agent/{agent.go,agent_test.go}: outbound reconnect, heartbeat, one active task, renew/cancel behavior.
- deploy/, README.md: production example, local startup, limitations and acceptance commands.

## Task 1: Durable authority and fencing

- [x] Write tests: idempotency conflict, tenant lookup, exclusive claim, expired results, old connection result, output flag, cancel and reopen persistence.
- [x] Run `go test ./internal/store`; confirm failure for missing implementation.
- [x] Implement schema and transactional operations with SQL conditions, unique keys and numeric fencing.
- [x] Run store tests; verify all pass.

## Task 2: Real model backend

- [x] Write tests using httptest model endpoint: request preservation, streaming chunks, structured errors, missing DONE, cancellation.
- [x] Run backend tests and observe missing implementation failure.
- [x] Implement context-aware OpenAI-compatible HTTP/SSE adapter with bounded line lengths and no silent error suppression.
- [x] Run backend tests; verify all pass.

## Task 3: Authenticated control and Agent transport

- [x] Write real network tests: enrolled Agent → authenticated chat/batch → backend, isolation, idempotency, cancel, interrupted stream and reconnect fencing.
- [x] Run tests and observe missing implementation failure.
- [x] Implement APIs, WebSocket protocol, SQL dispatch/recovery, cancellation and local Agent execution.
- [x] Run all tests including concurrent claims and shutdown; resolve failures before next task.

## Task 4: Executables and deployment

- [x] Add binaries, flags, configuration validation, local demo backend explicitly marked simulated, production Compose and SQL migration documentation.
- [x] Run whole suite, vet, build, race tests where host supports them; run executable smoke test with real HTTP transport and dev simulator.
- [x] Independent reviewer examines implementation/security; reproduce meaningful findings as failing tests then fix.
- [x] Record actual results and unimplemented design stages in README and progress ledger.

## Scope accounting

This increment implements the reliable vertical slice of P1/P2. GPU profiling, fair multi-tenant queues beyond bounded admission, tiered KV, NATS outbox, agent SQLite WAL, download distribution, DeepInfer native engine, multi-relay HA and full P0 hardware/72h performance acceptance are not claimed complete. Their scope remains the approved design.
