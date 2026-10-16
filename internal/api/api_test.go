package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Validation runs before any database access, so a Server with no store can
// exercise every 400 path.
func TestRequestValidation(t *testing.T) {
	h := (&Server{}).Handler()
	id := "00000000-0000-4000-8000-000000000001"
	id2 := "00000000-0000-4000-8000-000000000002"
	cases := []struct {
		name, path, body, key string
		code                  int
	}{
		{"empty body", "/v1/transfers", "", "", 400},
		{"malformed json", "/v1/transfers", "{", "", 400},
		{"unknown field", "/v1/transfers", `{"bogus":1}`, "", 400},
		{"trailing data", "/v1/accounts", `{"name":"a","currency":"USD"}{}`, "", 400},
		{"non-uuid", "/v1/transfers", `{"from_account_id":"x","to_account_id":"y","amount":1,"currency":"USD"}`, "", 400},
		{"negative amount", "/v1/transfers", `{"from_account_id":"` + id + `","to_account_id":"` + id2 + `","amount":-1,"currency":"USD"}`, "", 400},
		{"float amount", "/v1/transfers", `{"from_account_id":"` + id + `","to_account_id":"` + id2 + `","amount":1.5,"currency":"USD"}`, "", 400},
		{"lowercase currency", "/v1/accounts", `{"name":"a","currency":"usd"}`, "", 400},
		{"missing name", "/v1/accounts", `{"currency":"USD"}`, "", 400},
		{"key too long", "/v1/accounts", `{"name":"a","currency":"USD"}`, strings.Repeat("k", 256), 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			if tc.key != "" {
				req.Header.Set("Idempotency-Key", tc.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.code {
				t.Fatalf("got %d %s", rec.Code, rec.Body)
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Fatalf("content-type %q", ct)
			}
		})
	}
}

func TestUnknownAccountIDIs404WithoutDB(t *testing.T) {
	h := (&Server{}).Handler()
	for _, p := range []string{"/v1/accounts/nope", "/v1/accounts/nope/balance", "/v1/accounts/nope/entries"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, p, nil))
		if rec.Code != 404 {
			t.Errorf("%s: %d", p, rec.Code)
		}
	}
}
