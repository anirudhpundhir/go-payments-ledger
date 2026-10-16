-- Accounts hold a denormalised balance kept in sync with postings inside the
-- same transaction. The CHECK is a last line of defence against overdraft.
CREATE TABLE accounts (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT        NOT NULL,
    currency       CHAR(3)     NOT NULL CHECK (currency ~ '^[A-Z]{3}$'),
    balance        BIGINT      NOT NULL DEFAULT 0,
    allow_negative BOOLEAN     NOT NULL DEFAULT FALSE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT balance_non_negative CHECK (allow_negative OR balance >= 0)
);

CREATE TABLE journal_entries (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    description TEXT        NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Postings are append-only. amount > 0 credits, amount < 0 debits.
CREATE TABLE postings (
    id         BIGSERIAL PRIMARY KEY,
    entry_id   UUID    NOT NULL REFERENCES journal_entries (id),
    account_id UUID    NOT NULL REFERENCES accounts (id),
    amount     BIGINT  NOT NULL CHECK (amount <> 0),
    currency   CHAR(3) NOT NULL,
    UNIQUE (entry_id, account_id)
);
CREATE INDEX postings_account_id_id ON postings (account_id, id DESC);
