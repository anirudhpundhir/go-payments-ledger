package integration

import (
	"encoding/json"
	"testing"

	"github.com/anirudhpundhir/go-payments-ledger/internal/idempotency"
)

func fp(t *testing.T, path string, body any) []byte {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return idempotency.Fingerprint("POST", path, b)
}
