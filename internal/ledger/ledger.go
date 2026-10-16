// Package ledger contains the pure double-entry domain model: accounts,
// journal entries and postings. It has no I/O and is fully unit-testable.
package ledger

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

// Domain errors. The API layer maps these to HTTP status codes.
var (
	ErrInvalidAmount      = errors.New("amount must be a positive integer number of minor units")
	ErrInvalidCurrency    = errors.New("currency must be a 3-letter uppercase ISO 4217 code")
	ErrCurrencyMismatch   = errors.New("posting currency does not match account currency")
	ErrUnbalanced         = errors.New("postings do not balance to zero per currency")
	ErrTooFewPostings     = errors.New("a journal entry needs at least two postings")
	ErrSameAccount        = errors.New("source and destination accounts must differ")
	ErrInsufficientFunds  = errors.New("insufficient funds")
	ErrAccountNotFound    = errors.New("account not found")
	ErrZeroPosting        = errors.New("posting amount must be non-zero")
	ErrDuplicateAccountID = errors.New("account appears more than once in entry")
)

var currencyRE = regexp.MustCompile(`^[A-Z]{3}$`)

// ValidateCurrency checks the ISO-4217 shape of a currency code.
func ValidateCurrency(c string) error {
	if !currencyRE.MatchString(c) {
		return ErrInvalidCurrency
	}
	return nil
}

// Account is a ledger account. Balance is in integer minor units (e.g. cents)
// and is a denormalised cache of SUM(postings.amount) for the account.
type Account struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Currency      string    `json:"currency"`
	Balance       int64     `json:"balance"`
	AllowNegative bool      `json:"allow_negative"`
	CreatedAt     time.Time `json:"created_at"`
}

// Posting is one leg of a journal entry. Positive amounts credit (increase)
// the account balance, negative amounts debit (decrease) it.
type Posting struct {
	AccountID string `json:"account_id"`
	Amount    int64  `json:"amount"`
	Currency  string `json:"currency"`
}

// Entry is an immutable, balanced journal entry.
type Entry struct {
	ID          string    `json:"id"`
	Description string    `json:"description"`
	Postings    []Posting `json:"postings"`
	CreatedAt   time.Time `json:"created_at"`
}

// ValidatePostings enforces the double-entry invariants that don't need
// account state: >=2 legs, non-zero legs, each account once, and the sum of
// amounts is zero independently for every currency (no implicit FX).
func ValidatePostings(ps []Posting) error {
	if len(ps) < 2 {
		return ErrTooFewPostings
	}
	sums := map[string]int64{}
	seen := map[string]bool{}
	for _, p := range ps {
		if p.Amount == 0 {
			return ErrZeroPosting
		}
		if err := ValidateCurrency(p.Currency); err != nil {
			return err
		}
		if seen[p.AccountID] {
			return ErrDuplicateAccountID
		}
		seen[p.AccountID] = true
		s, ok := addChecked(sums[p.Currency], p.Amount)
		if !ok {
			return fmt.Errorf("%w: overflow", ErrInvalidAmount)
		}
		sums[p.Currency] = s
	}
	for _, s := range sums {
		if s != 0 {
			return ErrUnbalanced
		}
	}
	return nil
}

// ApplyPosting returns the new balance of acct after p, enforcing currency
// match, overflow safety and the no-overdraft rule.
func ApplyPosting(acct Account, p Posting) (int64, error) {
	if acct.Currency != p.Currency {
		return 0, ErrCurrencyMismatch
	}
	nb, ok := addChecked(acct.Balance, p.Amount)
	if !ok {
		return 0, fmt.Errorf("%w: overflow", ErrInvalidAmount)
	}
	if nb < 0 && !acct.AllowNegative {
		return 0, ErrInsufficientFunds
	}
	return nb, nil
}

// TransferPostings builds the two legs of a simple transfer.
func TransferPostings(from, to string, amount int64, currency string) ([]Posting, error) {
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	if from == to {
		return nil, ErrSameAccount
	}
	if err := ValidateCurrency(currency); err != nil {
		return nil, err
	}
	return []Posting{
		{AccountID: from, Amount: -amount, Currency: currency},
		{AccountID: to, Amount: amount, Currency: currency},
	}, nil
}

func addChecked(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}
