package idempotency

import (
	"bytes"
	"regexp"
	"testing"
)

func TestFingerprint(t *testing.T) {
	a := Fingerprint("POST", "/v1/transfers", []byte(`{"amount":1}`))
	if !bytes.Equal(a, Fingerprint("POST", "/v1/transfers", []byte(`{"amount":1}`))) {
		t.Fatal("not deterministic")
	}
	for _, other := range [][]byte{
		Fingerprint("POST", "/v1/transfers", []byte(`{"amount":2}`)),
		Fingerprint("POST", "/v1/accounts", []byte(`{"amount":1}`)),
		Fingerprint("PUT", "/v1/transfers", []byte(`{"amount":1}`)),
	} {
		if bytes.Equal(a, other) {
			t.Fatal("collision")
		}
	}
}

func TestNewTokenIsUUIDv4(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	seen := map[string]bool{}
	for i := 0; i < 1000; i++ {
		tok, err := newToken()
		if err != nil || !re.MatchString(tok) || seen[tok] {
			t.Fatalf("bad token %q %v", tok, err)
		}
		seen[tok] = true
	}
}
