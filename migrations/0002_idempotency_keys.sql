-- Stripe-style idempotency keys. A row is inserted (status=in_progress) before
-- the operation runs and flipped to completed in the SAME transaction as the
-- operation's side effects, so "completed" implies "effects committed".
CREATE TABLE idempotency_keys (
    key            TEXT        PRIMARY KEY,
    request_hash   BYTEA       NOT NULL,
    status         TEXT        NOT NULL CHECK (status IN ('in_progress', 'completed')),
    response_code  INT,
    response_body  BYTEA,
    lock_token     UUID,
    locked_until   TIMESTAMPTZ,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at   TIMESTAMPTZ
);
CREATE INDEX idempotency_keys_created_at ON idempotency_keys (created_at);
