// Package store implements Postgres persistence for the ledger.
//
// Every write runs in a SERIALIZABLE transaction and additionally takes
// row locks (SELECT ... FOR UPDATE, in a deterministic id order) on the
// accounts it touches. Serialization failures and deadlocks are retried
// with jittered exponential backoff.
package store

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/anirudhpundhir/go-payments-ledger/internal/ledger"
	"github.com/anirudhpundhir/go-payments-ledger/migrations"
)

// MaxAttempts bounds optimistic retries of a serializable transaction.
const MaxAttempts = 10

// Store wraps a pgx connection pool.
type Store struct {
	Pool *pgxpool.Pool
}

// Open connects to Postgres and verifies connectivity.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool.
func (s *Store) Close() { s.Pool.Close() }

// Migrate applies pending embedded migrations, serialised by an advisory lock
// so concurrent instances can start safely.
func (s *Store) Migrate(ctx context.Context) error {
	conn, err := s.Pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(727274)`); err != nil {
		return err
	}
	defer func() { _, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727274)`) }()
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

// IsRetryable reports whether err is a serialization failure or deadlock.
func IsRetryable(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && (pgErr.Code == "40001" || pgErr.Code == "40P01")
}

// WithTx runs fn in a SERIALIZABLE transaction, retrying on serialization
// failures. fn must be safe to re-run (no side effects outside tx).
func (s *Store) WithTx(ctx context.Context, fn func(pgx.Tx) error) error {
	var err error
	for attempt := 0; attempt < MaxAttempts; attempt++ {
		err = pgx.BeginTxFunc(ctx, s.Pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		if !IsRetryable(err) {
			return err
		}
		backoff := time.Duration(1<<attempt) * time.Millisecond
		backoff += time.Duration(rand.Int64N(int64(backoff) + 1))
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
	return fmt.Errorf("transaction failed after %d attempts: %w", MaxAttempts, err)
}

// CreateAccount inserts a new account.
func CreateAccount(ctx context.Context, tx pgx.Tx, name, currency string, allowNegative bool) (ledger.Account, error) {
	if err := ledger.ValidateCurrency(currency); err != nil {
		return ledger.Account{}, err
	}
	var a ledger.Account
	err := tx.QueryRow(ctx, `INSERT INTO accounts (name, currency, allow_negative) VALUES ($1,$2,$3)
		RETURNING id::text, name, currency, balance, allow_negative, created_at`,
		name, currency, allowNegative).Scan(&a.ID, &a.Name, &a.Currency, &a.Balance, &a.AllowNegative, &a.CreatedAt)
	return a, err
}

// GetAccount loads an account without locking.
func GetAccount(ctx context.Context, q pgx.Tx, id string) (ledger.Account, error) {
	return scanAccount(q.QueryRow(ctx, `SELECT id::text, name, currency, balance, allow_negative, created_at
		FROM accounts WHERE id = $1`, id))
}

// GetAccountPool loads an account outside a transaction (read path).
func (s *Store) GetAccount(ctx context.Context, id string) (ledger.Account, error) {
	return scanAccount(s.Pool.QueryRow(ctx, `SELECT id::text, name, currency, balance, allow_negative, created_at
		FROM accounts WHERE id = $1`, id))
}

func scanAccount(row pgx.Row) (ledger.Account, error) {
	var a ledger.Account
	err := row.Scan(&a.ID, &a.Name, &a.Currency, &a.Balance, &a.AllowNegative, &a.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) || isInvalidUUID(err) {
		return a, ledger.ErrAccountNotFound
	}
	return a, err
}

func isInvalidUUID(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "22P02"
}

// PostEntry validates and atomically records a balanced journal entry,
// updating the cached balance of every touched account.
func PostEntry(ctx context.Context, tx pgx.Tx, description string, ps []ledger.Posting) (ledger.Entry, error) {
	if err := ledger.ValidatePostings(ps); err != nil {
		return ledger.Entry{}, err
	}
	// Lock accounts in a canonical order to avoid lock-order deadlocks.
	ids := make([]string, len(ps))
	for i, p := range ps {
		ids[i] = p.AccountID
	}
	sort.Strings(ids)
	rows, err := tx.Query(ctx, `SELECT id::text, name, currency, balance, allow_negative, created_at
		FROM accounts WHERE id = ANY($1::uuid[]) ORDER BY id FOR UPDATE`, ids)
	if err != nil {
		if isInvalidUUID(err) {
			return ledger.Entry{}, ledger.ErrAccountNotFound
		}
		return ledger.Entry{}, err
	}
	accts := map[string]ledger.Account{}
	for rows.Next() {
		a, err := scanAccount(rows)
		if err != nil {
			rows.Close()
			return ledger.Entry{}, err
		}
		accts[a.ID] = a
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		if isInvalidUUID(err) {
			return ledger.Entry{}, ledger.ErrAccountNotFound
		}
		return ledger.Entry{}, err
	}
	newBal := map[string]int64{}
	for _, p := range ps {
		a, ok := accts[p.AccountID]
		if !ok {
			return ledger.Entry{}, ledger.ErrAccountNotFound
		}
		nb, err := ledger.ApplyPosting(a, p)
		if err != nil {
			return ledger.Entry{}, err
		}
		newBal[a.ID] = nb
	}
	e := ledger.Entry{Description: description, Postings: ps}
	if err := tx.QueryRow(ctx, `INSERT INTO journal_entries (description) VALUES ($1) RETURNING id::text, created_at`,
		description).Scan(&e.ID, &e.CreatedAt); err != nil {
		return ledger.Entry{}, err
	}
	batch := &pgx.Batch{}
	for _, p := range ps {
		batch.Queue(`INSERT INTO postings (entry_id, account_id, amount, currency) VALUES ($1,$2,$3,$4)`,
			e.ID, p.AccountID, p.Amount, p.Currency)
		batch.Queue(`UPDATE accounts SET balance = $2 WHERE id = $1`, p.AccountID, newBal[p.AccountID])
	}
	if err := tx.SendBatch(ctx, batch).Close(); err != nil {
		return ledger.Entry{}, err
	}
	return e, nil
}

// EntryLine is one posting from the perspective of a single account.
type EntryLine struct {
	PostingID   int64     `json:"posting_id"`
	EntryID     string    `json:"entry_id"`
	Description string    `json:"description"`
	Amount      int64     `json:"amount"`
	Currency    string    `json:"currency"`
	CreatedAt   time.Time `json:"created_at"`
}

// ListEntries returns an account's postings newest-first, keyset-paginated
// by posting id (pass before=0 for the first page).
func (s *Store) ListEntries(ctx context.Context, accountID string, before int64, limit int) ([]EntryLine, error) {
	if _, err := s.GetAccount(ctx, accountID); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT p.id, p.entry_id::text, e.description, p.amount, p.currency, e.created_at
		FROM postings p JOIN journal_entries e ON e.id = p.entry_id
		WHERE p.account_id = $1 AND ($2 = 0 OR p.id < $2)
		ORDER BY p.id DESC LIMIT $3`, accountID, before, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (EntryLine, error) {
		var l EntryLine
		err := r.Scan(&l.PostingID, &l.EntryID, &l.Description, &l.Amount, &l.Currency, &l.CreatedAt)
		return l, err
	})
}
