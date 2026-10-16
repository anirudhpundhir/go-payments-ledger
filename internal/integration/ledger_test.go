package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
)

func TestTransferLifecycle(t *testing.T) {
	e := setup(t)
	src, alice := e.fund(1_000)
	bob := e.account("bob", "USD", false)

	if r := e.do("POST", "/v1/transfers", "", transfer(alice, bob, 250, "USD")); r.Code != http.StatusCreated {
		t.Fatalf("transfer: %d %s", r.Code, r.Body)
	}
	r := e.do("GET", "/v1/accounts/"+alice+"/balance", "", nil)
	var bal struct {
		Balance  int64
		Currency string
	}
	_ = json.Unmarshal(r.Body, &bal)
	if r.Code != 200 || bal.Balance != 750 || bal.Currency != "USD" {
		t.Fatalf("balance: %d %s", r.Code, r.Body)
	}
	if e.balance(bob) != 250 || e.balance(src) != -1_000 {
		t.Fatalf("balances: bob=%d src=%d", e.balance(bob), e.balance(src))
	}

	r = e.do("GET", "/v1/accounts/"+alice+"/entries?limit=1", "", nil)
	var page struct {
		Data []struct {
			Amount int64
		}
		HasMore    bool  `json:"has_more"`
		NextBefore int64 `json:"next_before"`
	}
	_ = json.Unmarshal(r.Body, &page)
	if len(page.Data) != 1 || page.Data[0].Amount != -250 || !page.HasMore {
		t.Fatalf("entries page 1: %s", r.Body)
	}
	r = e.do("GET", fmt.Sprintf("/v1/accounts/%s/entries?limit=1&before=%d", alice, page.NextBefore), "", nil)
	_ = json.Unmarshal(r.Body, &page)
	if len(page.Data) != 1 || page.Data[0].Amount != 1_000 {
		t.Fatalf("entries page 2: %s", r.Body)
	}
	e.assertInvariants()
}

func TestTransferRejections(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(100)
	bob := e.account("bob", "USD", false)
	eur := e.account("eur", "EUR", false)
	missing := "00000000-0000-4000-8000-000000000000"

	cases := []struct {
		name string
		body any
		code int
		typ  string
	}{
		{"insufficient funds", transfer(alice, bob, 101, "USD"), 422, "insufficient_funds"},
		{"currency mismatch", transfer(alice, eur, 10, "USD"), 422, "currency_mismatch"},
		{"wrong currency on source", transfer(alice, bob, 10, "EUR"), 422, "currency_mismatch"},
		{"unknown account", transfer(alice, missing, 10, "USD"), 404, "not_found"},
		{"zero amount", transfer(alice, bob, 0, "USD"), 400, "invalid_request"},
		{"same account", transfer(alice, alice, 1, "USD"), 400, "invalid_request"},
		{"bad id", transfer("nope", bob, 1, "USD"), 400, "invalid_request"},
		{"unknown field", map[string]any{"amount": 1, "foo": 1}, 400, "invalid_request"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := e.do("POST", "/v1/transfers", "", tc.body)
			var b struct{ Error struct{ Type string } }
			_ = json.Unmarshal(r.Body, &b)
			if r.Code != tc.code || b.Error.Type != tc.typ {
				t.Fatalf("got %d %s", r.Code, r.Body)
			}
		})
	}
	if e.balance(alice) != 100 || e.balance(bob) != 0 {
		t.Fatal("rejected transfers moved money")
	}
	e.assertInvariants()
}

// TestNoDoubleSpend fires many concurrent withdrawals at one account. Exactly
// balance/amount must succeed and the balance must never go negative.
func TestNoDoubleSpend(t *testing.T) {
	e := setup(t)
	const (
		balance  = 1_000
		amount   = 100
		workers  = 64
		expectOK = balance / amount
	)
	_, alice := e.fund(balance)
	sinks := make([]string, 4)
	for i := range sinks {
		sinks[i] = e.account(fmt.Sprintf("sink-%d", i), "USD", false)
	}
	var ok, rejected, other atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			r := e.do("POST", "/v1/transfers", "", transfer(alice, sinks[i%len(sinks)], amount, "USD"))
			switch r.Code {
			case http.StatusCreated:
				ok.Add(1)
			case http.StatusUnprocessableEntity:
				rejected.Add(1)
			default:
				other.Add(1)
				t.Errorf("unexpected %d %s", r.Code, r.Body)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if ok.Load() != expectOK || rejected.Load() != workers-expectOK || other.Load() != 0 {
		t.Fatalf("ok=%d rejected=%d other=%d", ok.Load(), rejected.Load(), other.Load())
	}
	if b := e.balance(alice); b != 0 {
		t.Fatalf("final balance %d, want 0", b)
	}
	e.assertInvariants()
}

// TestConcurrentOpposingTransfers exercises lock ordering: A->B and B->A at
// the same time must neither deadlock-fail nor lose money.
func TestConcurrentOpposingTransfers(t *testing.T) {
	e := setup(t)
	src, a := e.fund(10_000)
	b := e.account("b", "USD", false)
	if r := e.do("POST", "/v1/transfers", "", transfer(src, b, 10_000, "USD")); r.Code != 201 {
		t.Fatal(string(r.Body))
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); e.mustTransfer(a, b, 7) }()
		go func() { defer wg.Done(); e.mustTransfer(b, a, 7) }()
	}
	wg.Wait()
	if e.balance(a) != 10_000 || e.balance(b) != 10_000 {
		t.Fatalf("a=%d b=%d", e.balance(a), e.balance(b))
	}
	e.assertInvariants()
}

func (e *env) mustTransfer(from, to string, amt int64) {
	if r := e.do("POST", "/v1/transfers", "", transfer(from, to, amt, "USD")); r.Code != http.StatusCreated {
		e.t.Errorf("transfer: %d %s", r.Code, r.Body)
	}
}
