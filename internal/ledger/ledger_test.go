package ledger

import (
	"errors"
	"math"
	"testing"
)

func TestValidatePostings(t *testing.T) {
	cases := []struct {
		name string
		ps   []Posting
		want error
	}{
		{"balanced", []Posting{{"a", -100, "USD"}, {"b", 100, "USD"}}, nil},
		{"three legs", []Posting{{"a", -100, "USD"}, {"b", 60, "USD"}, {"c", 40, "USD"}}, nil},
		{"multi-currency each balanced", []Posting{{"a", -1, "USD"}, {"b", 1, "USD"}, {"c", -5, "EUR"}, {"d", 5, "EUR"}}, nil},
		{"one leg", []Posting{{"a", 100, "USD"}}, ErrTooFewPostings},
		{"unbalanced", []Posting{{"a", -100, "USD"}, {"b", 99, "USD"}}, ErrUnbalanced},
		{"cross-currency nets to zero but not per currency", []Posting{{"a", -100, "USD"}, {"b", 100, "EUR"}}, ErrUnbalanced},
		{"zero leg", []Posting{{"a", 0, "USD"}, {"b", 0, "USD"}}, ErrZeroPosting},
		{"bad currency", []Posting{{"a", -1, "usd"}, {"b", 1, "usd"}}, ErrInvalidCurrency},
		{"dup account", []Posting{{"a", -1, "USD"}, {"a", 1, "USD"}}, ErrDuplicateAccountID},
		{"overflow", []Posting{{"a", math.MaxInt64, "USD"}, {"b", 1, "USD"}, {"c", -1, "USD"}}, ErrInvalidAmount},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidatePostings(tc.ps); !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
}

func TestApplyPosting(t *testing.T) {
	acct := Account{ID: "a", Currency: "USD", Balance: 50}
	if nb, err := ApplyPosting(acct, Posting{"a", -50, "USD"}); err != nil || nb != 0 {
		t.Fatalf("drain to zero: %d %v", nb, err)
	}
	if _, err := ApplyPosting(acct, Posting{"a", -51, "USD"}); !errors.Is(err, ErrInsufficientFunds) {
		t.Fatalf("overdraft: %v", err)
	}
	if _, err := ApplyPosting(acct, Posting{"a", 1, "EUR"}); !errors.Is(err, ErrCurrencyMismatch) {
		t.Fatalf("currency: %v", err)
	}
	acct.AllowNegative = true
	if nb, err := ApplyPosting(acct, Posting{"a", -100, "USD"}); err != nil || nb != -50 {
		t.Fatalf("allow negative: %d %v", nb, err)
	}
	acct.Balance = math.MaxInt64
	if _, err := ApplyPosting(acct, Posting{"a", 1, "USD"}); !errors.Is(err, ErrInvalidAmount) {
		t.Fatalf("overflow: %v", err)
	}
}

func TestTransferPostings(t *testing.T) {
	ps, err := TransferPostings("a", "b", 10, "USD")
	if err != nil || ValidatePostings(ps) != nil {
		t.Fatalf("valid transfer: %v", err)
	}
	for _, tc := range []struct {
		from, to string
		amt      int64
		cur      string
		want     error
	}{
		{"a", "b", 0, "USD", ErrInvalidAmount},
		{"a", "b", -5, "USD", ErrInvalidAmount},
		{"a", "a", 5, "USD", ErrSameAccount},
		{"a", "b", 5, "US", ErrInvalidCurrency},
	} {
		if _, err := TransferPostings(tc.from, tc.to, tc.amt, tc.cur); !errors.Is(err, tc.want) {
			t.Errorf("%+v: got %v", tc, err)
		}
	}
}
