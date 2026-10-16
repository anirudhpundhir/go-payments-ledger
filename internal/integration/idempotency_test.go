package integration

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"testing"
)

func TestIdempotentReplay(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(1_000)
	bob := e.account("bob", "USD", false)

	first := e.do("POST", "/v1/transfers", "key-1", transfer(alice, bob, 100, "USD"))
	if first.Code != 201 || first.Replayed {
		t.Fatalf("first: %d %s", first.Code, first.Body)
	}
	second := e.do("POST", "/v1/transfers", "key-1", transfer(alice, bob, 100, "USD"))
	if second.Code != 201 || !second.Replayed || !bytes.Equal(first.Body, second.Body) {
		t.Fatalf("replay differs: %d replayed=%v\n%s\n%s", second.Code, second.Replayed, first.Body, second.Body)
	}
	if e.balance(alice) != 900 {
		t.Fatalf("replay moved money again: %d", e.balance(alice))
	}
	e.assertInvariants()
}

func TestIdempotentReplayOfBusinessError(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(50)
	bob := e.account("bob", "USD", false)
	r1 := e.do("POST", "/v1/transfers", "key-err", transfer(alice, bob, 100, "USD"))
	if r1.Code != 422 {
		t.Fatalf("want 422, got %d", r1.Code)
	}
	// Even after funds arrive, the same key replays the original outcome.
	src := e.account("ext", "USD", true)
	e.mustTransfer(src, alice, 500)
	r2 := e.do("POST", "/v1/transfers", "key-err", transfer(alice, bob, 100, "USD"))
	if r2.Code != 422 || !r2.Replayed || !bytes.Equal(r1.Body, r2.Body) {
		t.Fatalf("got %d replayed=%v %s", r2.Code, r2.Replayed, r2.Body)
	}
	if e.balance(bob) != 0 {
		t.Fatal("money moved")
	}
}

func TestIdempotencyKeyReuseWithDifferentBody(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(1_000)
	bob := e.account("bob", "USD", false)
	if r := e.do("POST", "/v1/transfers", "key-2", transfer(alice, bob, 100, "USD")); r.Code != 201 {
		t.Fatal(string(r.Body))
	}
	r := e.do("POST", "/v1/transfers", "key-2", transfer(alice, bob, 200, "USD"))
	if r.Code != http.StatusUnprocessableEntity {
		t.Fatalf("want 422, got %d %s", r.Code, r.Body)
	}
	// Same key on a different endpoint is also a mismatch.
	if r := e.do("POST", "/v1/accounts", "key-2", map[string]any{"name": "x", "currency": "USD"}); r.Code != 422 {
		t.Fatalf("cross-endpoint reuse: %d", r.Code)
	}
	if e.balance(alice) != 900 {
		t.Fatal("mismatched request executed")
	}
}

func TestIdempotencyKeyInProgress(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(1_000)
	bob := e.account("bob", "USD", false)
	body := transfer(alice, bob, 100, "USD")

	// Simulate a live in-flight request holding the key: issue the request
	// once to learn the fingerprint shape, then plant an in-progress claim.
	ctx := context.Background()
	if _, err := e.store.Pool.Exec(ctx, `INSERT INTO idempotency_keys (key, request_hash, status, lock_token, locked_until)
		VALUES ('key-busy', $1, 'in_progress', gen_random_uuid(), now() + interval '1 minute')`, fp(t, "/v1/transfers", body)); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/transfers", "key-busy", body); r.Code != http.StatusConflict {
		t.Fatalf("want 409, got %d %s", r.Code, r.Body)
	}
	// An expired lease (crashed owner) is safely re-claimed and executed once.
	if _, err := e.store.Pool.Exec(ctx, `UPDATE idempotency_keys SET locked_until = now() - interval '1 second' WHERE key='key-busy'`); err != nil {
		t.Fatal(err)
	}
	if r := e.do("POST", "/v1/transfers", "key-busy", body); r.Code != 201 || r.Replayed {
		t.Fatalf("reclaim: %d %s", r.Code, r.Body)
	}
	if e.balance(alice) != 900 {
		t.Fatalf("balance %d", e.balance(alice))
	}
}

// TestConcurrentSameKey races many identical requests with one key: the
// transfer must execute exactly once; losers see 409 (in progress) or a
// replay of the winner's response.
func TestConcurrentSameKey(t *testing.T) {
	e := setup(t)
	_, alice := e.fund(1_000)
	bob := e.account("bob", "USD", false)
	const n = 32
	results := make([]result, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = e.do("POST", "/v1/transfers", "race-key", transfer(alice, bob, 100, "USD"))
		}(i)
	}
	close(start)
	wg.Wait()
	fresh, conflicts, replays := 0, 0, 0
	for _, r := range results {
		switch {
		case r.Code == 201 && !r.Replayed:
			fresh++
		case r.Code == 201 && r.Replayed:
			replays++
		case r.Code == 409:
			conflicts++
		default:
			t.Fatalf("unexpected %d %s", r.Code, r.Body)
		}
	}
	if fresh != 1 {
		t.Fatalf("fresh executions = %d, want 1 (409=%d replay=%d)", fresh, conflicts, replays)
	}
	t.Logf("fresh=%d conflicts(409)=%d replays=%d", fresh, conflicts, replays)
	if e.balance(alice) != 900 {
		t.Fatalf("balance %d, want 900", e.balance(alice))
	}
	e.assertInvariants()
}
