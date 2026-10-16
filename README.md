# go-payments-ledger

[![CI](https://github.com/anirudhpundhir/go-payments-ledger/actions/workflows/ci.yml/badge.svg)](https://github.com/anirudhpundhir/go-payments-ledger/actions/workflows/ci.yml)

A double-entry payments ledger in Go, backed by Postgres, with **Stripe-style `Idempotency-Key` semantics**. It's small enough to read in one sitting, and it focuses on the parts that are hard to get right: never losing or duplicating money under concurrency, retries and crashes.

- **Double-entry**: every journal entry has at least 2 postings that sum to zero *per currency*. Amounts are `int64` minor units, and every account has exactly one currency.
- **No double spend**: row locks (`SELECT … FOR UPDATE`) are taken in a canonical order and checked by a concurrency test. Transactions retry on serialization failures and deadlocks.
- **Idempotency**: a replay returns the original response byte-for-byte. Reusing a key with a different body returns `422`; a concurrent duplicate returns `409`. A key is marked completed in the same transaction as the money movement, so it's crash-safe.
- **Production details**: embedded migrations under an advisory lock, graceful shutdown, structured logs, strict JSON decoding, a distroless image, and CI that runs the suite under both READ COMMITTED and SERIALIZABLE.

## Architecture

```mermaid
flowchart LR
    C[Client] -->|POST /v1/transfers<br/>Idempotency-Key: K| H[HTTP API<br/>internal/api]
    H -->|validate + fingerprint| I[Idempotency manager<br/>internal/idempotency]
    I -->|tx1: claim K<br/>in_progress + lease + token| PG[(Postgres)]
    I -->|tx2: op + mark K completed<br/>atomically| S[Store<br/>internal/store]
    S -->|FOR UPDATE accounts in id order<br/>insert entry + postings<br/>update balances| PG
    S -.uses.-> D[Domain<br/>internal/ledger<br/>pure invariants]
```

```mermaid
sequenceDiagram
    participant C as Client
    participant A as API
    participant DB as Postgres
    C->>A: POST /v1/transfers (K, body)
    A->>DB: INSERT idempotency_keys(K, sha256, in_progress, token) ON CONFLICT ...
    alt new key (or expired lease)
        A->>DB: BEGIN; SAVEPOINT; lock accounts; post entry; UPDATE key=completed WHERE token; COMMIT
        A-->>C: 201 (stored)
    else completed, same fingerprint
        A-->>C: stored response + Idempotent-Replayed: true
    else different fingerprint
        A-->>C: 422 idempotency_error
    else in progress
        A-->>C: 409 idempotency_error
    end
```

Layout:

```
cmd/ledgerd/            server entrypoint (config, migrations, graceful shutdown, key sweeper)
internal/ledger/        pure domain: postings validation, balance application (unit tests)
internal/store/         Postgres: migrations runner, transactions + retry, locking, queries
internal/idempotency/   claim / execute / complete protocol
internal/api/           HTTP handlers, error mapping
internal/integration/   end-to-end tests + benchmarks against real Postgres
migrations/             SQL schema (embedded)
docs/DESIGN.md          ADRs: invariants, locking, idempotency, trade-offs
```

## Quick start

```bash
docker compose up -d --build          # Postgres 17 + ledgerd on :8080
# or: start Postgres yourself, then
DATABASE_URL=postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable make run
```

## API

All amounts are integers in minor units. Errors look like `{"error":{"type":"...","message":"..."}}`.

| Method | Path | Notes |
|---|---|---|
| `POST` | `/v1/accounts` | `{name, currency, allow_negative?}` → `201`. Accepts `Idempotency-Key`. |
| `POST` | `/v1/transfers` | `{from_account_id, to_account_id, amount, currency, description?}` → `201` journal entry. Accepts `Idempotency-Key`. |
| `GET` | `/v1/accounts/{id}` | account |
| `GET` | `/v1/accounts/{id}/balance` | `{account_id, balance, currency}` |
| `GET` | `/v1/accounts/{id}/entries?limit=50&before=<posting_id>` | newest first, keyset pagination (`has_more`, `next_before`) |
| `GET` | `/healthz` | `204` when the DB is reachable |

Money enters the system from an account with `allow_negative: true` (an "external funding" account, the usual ledger convention), so the whole ledger always sums to zero.

### Examples (real output from a local run)

```bash
$ curl -s -XPOST localhost:8080/v1/accounts -d '{"name":"external-funding","currency":"USD","allow_negative":true}'
$ curl -s -XPOST localhost:8080/v1/accounts -d '{"name":"alice","currency":"USD"}'

$ curl -si -XPOST localhost:8080/v1/transfers -H 'Idempotency-Key: k1' \
    -d '{"from_account_id":"568f54ee-...","to_account_id":"01f69eb7-...","amount":5000,"currency":"USD","description":"top-up"}'
HTTP/1.1 201 Created
{"id":"fb1b0c1f-...","description":"top-up","postings":[{"account_id":"568f54ee-...","amount":-5000,"currency":"USD"},{"account_id":"01f69eb7-...","amount":5000,"currency":"USD"}],"created_at":"..."}

# same key, same body → identical response, no second movement
HTTP/1.1 201 Created
Idempotent-Replayed: true

# same key, different body
HTTP/1.1 422 Unprocessable Entity
{"error":{"type":"idempotency_error","message":"Keys for idempotent requests can only be used with the same parameters they were first used with."}}

$ curl -s localhost:8080/v1/accounts/01f69eb7-.../balance
{"account_id":"01f69eb7-...","balance":5000,"currency":"USD"}
```

Business errors: `422 insufficient_funds`, `422 currency_mismatch`, `404 not_found`, `400 invalid_request`.

## Guarantees

1. **Conservation**: each entry nets to zero per currency, and so does the ledger. This is enforced in the domain layer and asserted globally after every integration test.
2. **No overdraft / double spend**: a balance check and update happen under a row lock. The schema `CHECK` constraint is a backstop. `TestNoDoubleSpend` sends 64 concurrent 100-unit withdrawals at a 1,000 balance and asserts exactly 10 succeed and the final balance is 0.
3. **Deadlock freedom between transfers**: locks are taken in ascending account id order (`TestConcurrentOpposingTransfers`).
4. **At-most-once per idempotency key**: even with concurrent duplicates, crashes, or lease expiry (`TestConcurrentSameKey`, `TestIdempotencyKeyInProgress`). A `completed` key always means the effects were committed.
5. **Cached balances are exact**: `accounts.balance = SUM(postings.amount)`, asserted in tests.

## Trade-offs (details in [docs/DESIGN.md](docs/DESIGN.md))

- **READ COMMITTED + row locks by default, SERIALIZABLE optional.** Under SERIALIZABLE, Postgres aborts `FOR UPDATE` waiters on a recently updated row with `40001` instead of queueing them, so a hot account thrashes on retries. This showed up in the benchmark (see below), and it drove the decision.
- Single hot accounts are bounded by lock hold time. Sharding or batching hot accounts is a follow-up.
- Fingerprints hash raw bytes, as Stripe does: reordered JSON counts as a different request.
- Idempotency keys are global, because there's no auth or tenancy in scope.

## Testing

```bash
make test-unit            # no DB needed
docker compose up -d postgres
make test                 # unit + integration (race detector on)
make test-serializable    # integration suite under SERIALIZABLE
make lint                 # golangci-lint v2
```

The integration suite covers the transfer lifecycle and pagination, rejections (insufficient funds, currency mismatch, unknown account, validation), idempotent replay (success and business-error outcomes), key reuse with a different body or endpoint (422), in-progress keys (409), re-claiming an expired lease, concurrent same-key races, double-spend prevention, and opposing-direction deadlock avoidance. Statement coverage of `internal/...` from the full suite is about 76%.

## Benchmarks

These are end-to-end HTTP benchmarks (`make bench`). The client, server and Postgres all ran on the **same machine**: an 8 vCPU Intel Xeon VM with 15 GB RAM, Postgres 17.11 with the stock Debian config (fsync on), Go 1.25, `GOMAXPROCS=8`, `b.RunParallel` with 8 concurrent clients. I ran each benchmark 3 times with `-benchtime 3s`. These are illustrative numbers, not a capacity claim.

| Benchmark | READ COMMITTED (default) | SERIALIZABLE |
|---|---|---|
| `BenchmarkTransfer` (16 disjoint account pairs) | 129–141 µs/op (~7.1k–7.7k transfers/s) | 124–136 µs/op |
| `BenchmarkTransferIdempotent` (+ `Idempotency-Key`, 2 txns) | 184–206 µs/op (~4.9k–5.4k/s) | 181–190 µs/op |
| `BenchmarkTransferHotAccount` (all debits from one account) | 467–488 µs/op (~2.1k/s) | **fails**: `40001` after 10 retries |

## License

MIT
