# Design notes (ADR log)

Short Architecture Decision Records. Each has context, the decision, and its consequences.

---

## ADR-001: Double-entry with integer minor units, per-currency balancing

**Context.** A payments ledger must never create or destroy money. Floating point can't represent most decimal amounts exactly.

**Decision.**
- Amounts are `int64` **minor units** (cents for USD, yen for JPY). No floats in the API, domain, or schema (`BIGINT`).
- Every journal entry has ≥ 2 postings and **sums to zero within each currency**. A USD leg can't offset an EUR leg, so there's no implicit FX. A cross-currency movement has to be modelled explicitly, as two balanced entries through an FX/clearing account.
- Every account has exactly one currency. A posting whose currency differs from its account's is rejected (`422 currency_mismatch`).
- Postings are append-only. Corrections are new reversing entries; postings are never updated or deleted.
- Arithmetic is overflow-checked (`addChecked`).

**Consequences.** Global invariants are cheap to check (the integration suite asserts them after every test: each entry nets to zero, and each cached balance equals `SUM(postings)`). Minor-unit exponent metadata (e.g. 0 for JPY, 3 for KWD) is the client's job.

---

## ADR-002: Concurrency control: pessimistic row locks, READ COMMITTED by default, retries always on

**Context.** The core risk is a **double spend**: two concurrent debits each read balance=100 and both withdraw 100.

**Options considered.**
1. `SERIALIZABLE` (SSI) on its own, with optimistic retries.
2. `SELECT … FOR UPDATE` row locks under `READ COMMITTED`.
3. Both together.

**Measurement.** We first shipped option 3. `BenchmarkTransferHotAccount` (all transfers debit one account) failed: in Postgres, under `REPEATABLE READ`/`SERIALIZABLE`, `FOR UPDATE` on a row that another transaction changed after our snapshot raises `40001` instead of waiting. On a hot account, every waiter is aborted and retried, and 10 jittered retries weren't enough. Under `READ COMMITTED`, `FOR UPDATE` waits for the lock and then re-reads the latest row version, so waiters queue instead of thrashing.

**Decision.**
- Every balance-affecting write locks all touched accounts with `SELECT … FOR UPDATE` **in ascending id order** before reading balances. Canonical ordering prevents A→B / B→A deadlocks (`TestConcurrentOpposingTransfers`).
- The default isolation is `READ COMMITTED`. The lock makes the read-check-write of the balance atomic, which is all the invariant needs.
- `SERIALIZABLE` stays supported (`LEDGER_ISOLATION=serializable`), and CI runs the full integration suite under both.
- `WithTx` always retries `40001`/`40P01` with jittered exponential backoff (max 10 attempts). Operations are pure functions of the transaction, so re-running them is safe.
- Defence in depth: `CHECK (allow_negative OR balance >= 0)` in the schema.

**Consequences.** Throughput on a single hot account is bounded by lock hold time (one network round trip per batch). Hot accounts need different techniques; see the follow-ups. Under `SERIALIZABLE`, hot-account workloads can surface `500` after retry exhaustion. That's documented and measured.

---

## ADR-003: Idempotency keys (Stripe semantics)

**Context.** Clients retry on timeouts. Without idempotency, a retry after a lost response moves money twice.

**Semantics.**

| Situation | Response |
|---|---|
| First request with key K | Executes, stores `(fingerprint, status, body)` |
| Same K, same fingerprint, completed | Replays the stored status and body byte-for-byte, with `Idempotent-Replayed: true` |
| Same K, **different** fingerprint (body, path or method) | `422 idempotency_error` |
| Same K while the first is still executing | `409 idempotency_error` (retry later) |
| First attempt ended in a business error (e.g. `422 insufficient_funds`) | The error is stored and replayed, as Stripe does. A retry won't "succeed later" by surprise. |
| First attempt hit an infrastructure error (`5xx`, DB down) | The claim is released. The same key can be retried. |

The fingerprint is `SHA-256(method ‖ path ‖ raw body)`.

**Protocol (two transactions).**
1. **Claim**: `INSERT … ON CONFLICT DO UPDATE … WHERE status='in_progress' AND locked_until < now() AND request_hash = EXCLUDED.request_hash RETURNING`. This does one of three things atomically: claims a new key, re-claims one whose owner crashed (expired lease), or tells us someone else holds it. Each claim writes a random `lock_token`. The claim commits right away, so concurrent requests see it and get `409`.
2. **Execute + complete**: one transaction that runs the operation inside a savepoint and then runs `UPDATE idempotency_keys SET status='completed', response=… WHERE key=K AND lock_token=T`.
   - If the response is `4xx`, the savepoint is rolled back (no ledger effects), but the response is still recorded.
   - If the `UPDATE` matches 0 rows, our lease was stolen. The whole transaction rolls back, so the operation's effects are undone.

**Why this is crash-safe.** `completed` and the ledger effects commit atomically. A crash before commit leaves an `in_progress` row with no effects. After the lease (30 s) expires, a retry re-claims it and executes exactly once. A slow original holder that wakes up after losing the lease fails its token-guarded `UPDATE` and rolls back. At-most-once execution never depends on timing.

**Why not one transaction?** If the claim is never committed separately, concurrent duplicates block on the unique index until the first one commits, then replay. That works, but it ties up a connection per duplicate and doesn't give the required `409` signal.

**Retention.** A background sweeper deletes completed keys older than 24 h.

**Trade-offs.**
- Keys are global, not per-tenant (there's no auth in this project). A real system scopes keys by `(merchant_id, key)`.
- The fingerprint uses raw bytes, so semantically equal JSON with different whitespace or field order counts as a different request (422). Stripe behaves the same way. Canonicalising would be friendlier and costs CPU.
- A crashed request blocks its key for up to the lease duration (409s until then).

---

## ADR-004: Denormalised balances

**Decision.** `accounts.balance` is updated in the same transaction as the postings, under the row lock. Reads are O(1). `ListEntries` uses keyset pagination on `(account_id, id DESC)`.

**Consequences.** Balance drift is possible only through a bug, so the test suite checks `balance = SUM(postings)` for every account. A production system would also run that check as a periodic reconciliation job.

---

## ADR-005: Plain SQL with embedded migrations, pgx, stdlib router

**Decision.** `pgx/v5` with hand-written SQL, migrations embedded via `embed.FS` and applied at startup under a Postgres advisory lock (safe with N replicas), and the Go 1.22+ `http.ServeMux` method/path patterns. There's no ORM and no web framework.

**Consequences.** Locking and isolation behaviour stay explicit and reviewable. The project has two direct dependencies.

---

## ADR-006: Integration tests against real Postgres via env var, not testcontainers

**Decision.** Integration tests need `LEDGER_TEST_DATABASE_URL` and skip without it. Locally, `docker compose up postgres` provides one. In CI, a GitHub Actions service container does.

**Consequences.** Running the tests doesn't require a Docker daemon (useful in sandboxes and devcontainers). The cost is a manual step locally. Moving to `testcontainers-go` is a small follow-up if self-contained `go test` is preferred.

---

## Follow-ups / not implemented
- **Hot accounts**: sharded sub-accounts or batched/queued postings for platform accounts that are debited constantly.
- **AuthN/Z and tenant-scoped idempotency keys.**
- **Multi-leg entries over HTTP**: the domain and store support N-leg entries (`store.PostEntry`), but only `/v1/transfers` (2 legs) is exposed.
- **Reversals/refunds endpoint** (reversing entries).
- **Outbox/events** (e.g. `entry.created`) for downstream consumers.
- **Observability**: Prometheus metrics for retry counts, 409 rates and lock wait time; OpenTelemetry traces.
- **Request canonicalisation** for fingerprints.
