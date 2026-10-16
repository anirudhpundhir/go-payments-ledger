package integration

import (
	"fmt"
	"sync/atomic"
	"testing"
)

// BenchmarkTransfer measures end-to-end HTTP transfer throughput between
// disjoint account pairs (low contention).
func BenchmarkTransfer(b *testing.B) {
	benchTransfers(b, "", false)
}

// BenchmarkTransferIdempotent adds an Idempotency-Key to every request.
func BenchmarkTransferIdempotent(b *testing.B) {
	benchTransfers(b, "bench", false)
}

// BenchmarkTransferHotAccount sends every transfer from one account, the
// worst case for row-lock contention.
func BenchmarkTransferHotAccount(b *testing.B) {
	benchTransfers(b, "", true)
}

func benchTransfers(b *testing.B, keyPrefix string, hot bool) {
	e := setup(b)
	const pairs = 16
	froms, tos := make([]string, pairs), make([]string, pairs)
	for i := range froms {
		froms[i] = e.account("from", "USD", true)
		tos[i] = e.account("to", "USD", false)
	}
	var n atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			i := n.Add(1)
			from := froms[i%pairs]
			if hot {
				from = froms[0]
			}
			key := ""
			if keyPrefix != "" {
				key = fmt.Sprintf("%s-%d", keyPrefix, i)
			}
			if r := e.do("POST", "/v1/transfers", key, transfer(from, tos[i%pairs], 1, "USD")); r.Code != 201 {
				b.Errorf("%d %s", r.Code, r.Body)
			}
		}
	})
}
