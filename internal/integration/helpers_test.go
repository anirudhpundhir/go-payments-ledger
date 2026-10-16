// Package integration holds end-to-end tests against a real Postgres.
// Set LEDGER_TEST_DATABASE_URL (see Makefile / docker-compose.yml) to run them.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/anirudhpundhir/go-payments-ledger/internal/api"
	"github.com/anirudhpundhir/go-payments-ledger/internal/idempotency"
	"github.com/anirudhpundhir/go-payments-ledger/internal/store"
)

type env struct {
	t     testing.TB
	store *store.Store
	srv   *httptest.Server
}

func setup(t testing.TB) *env {
	t.Helper()
	dsn := os.Getenv("LEDGER_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("LEDGER_TEST_DATABASE_URL not set; skipping integration test")
	}
	ctx := context.Background()
	st, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := st.Migrate(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := st.Pool.Exec(ctx, `TRUNCATE postings, journal_entries, accounts, idempotency_keys`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	srv := httptest.NewServer((&api.Server{Store: st, Idem: idempotency.New(st, 30*time.Second)}).Handler())
	t.Cleanup(func() { srv.Close(); st.Close() })
	return &env{t: t, store: st, srv: srv}
}

type result struct {
	Code     int
	Body     []byte
	Replayed bool
}

func (e *env) do(method, path, key string, body any) result {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			e.t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, rdr)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return result{Code: resp.StatusCode, Body: b, Replayed: resp.Header.Get("Idempotent-Replayed") == "true"}
}

func (e *env) account(name, cur string, allowNeg bool) string {
	e.t.Helper()
	r := e.do("POST", "/v1/accounts", "", map[string]any{"name": name, "currency": cur, "allow_negative": allowNeg})
	if r.Code != http.StatusCreated {
		e.t.Fatalf("create account: %d %s", r.Code, r.Body)
	}
	var a struct{ ID string }
	_ = json.Unmarshal(r.Body, &a)
	return a.ID
}

func transfer(from, to string, amt int64, cur string) map[string]any {
	return map[string]any{"from_account_id": from, "to_account_id": to, "amount": amt, "currency": cur}
}

// fund creates a USD account holding amt, funded from an external source account.
func (e *env) fund(amt int64) (src, acct string) {
	e.t.Helper()
	src = e.account("external", "USD", true)
	acct = e.account("customer", "USD", false)
	if r := e.do("POST", "/v1/transfers", "", transfer(src, acct, amt, "USD")); r.Code != http.StatusCreated {
		e.t.Fatalf("fund: %d %s", r.Code, r.Body)
	}
	return src, acct
}

func (e *env) balance(id string) int64 {
	e.t.Helper()
	var a struct{ Balance int64 }
	if err := e.store.Pool.QueryRow(context.Background(), `SELECT balance FROM accounts WHERE id=$1`, id).Scan(&a.Balance); err != nil {
		e.t.Fatal(err)
	}
	return a.Balance
}

// assertInvariants checks the global double-entry invariants: every entry sums
// to zero, the whole ledger sums to zero, and every cached balance equals the
// sum of its postings.
func (e *env) assertInvariants() {
	e.t.Helper()
	ctx := context.Background()
	var n int
	if err := e.store.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT entry_id FROM postings GROUP BY entry_id, currency HAVING sum(amount) <> 0) x`).Scan(&n); err != nil || n != 0 {
		e.t.Fatalf("unbalanced entries: %d %v", n, err)
	}
	if err := e.store.Pool.QueryRow(ctx, `SELECT count(*) FROM accounts a WHERE a.balance <> COALESCE((SELECT sum(amount) FROM postings p WHERE p.account_id=a.id),0)`).Scan(&n); err != nil || n != 0 {
		e.t.Fatalf("balance cache drift on %d accounts: %v", n, err)
	}
}
