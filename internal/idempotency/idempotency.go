// Package idempotency implements Stripe-style Idempotency-Key semantics on
// top of Postgres.
//
// Protocol (see docs/DESIGN.md, ADR-003):
//
//  1. Claim: INSERT the key as in_progress with a random lock token and a
//     lease. If the key already exists:
//     - different request fingerprint      -> ErrMismatch   (HTTP 422)
//     - completed                          -> replay stored response
//     - in_progress with a live lease      -> ErrInProgress (HTTP 409)
//     - in_progress with an expired lease  -> re-claim (crashed owner)
//  2. Execute: run the operation AND mark the key completed with its response
//     in one transaction, guarded by the lock token. Therefore a
//     completed key always implies committed side effects, and a crash at any
//     point leaves either nothing or everything.
//  3. On infrastructure errors (not business 4xx) the claim is released so
//     the client may retry with the same key.
package idempotency

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anirudhpundhir/go-payments-ledger/internal/store"
)

var (
	// ErrMismatch means key reused with a different request.
	ErrMismatch = errors.New("idempotency key reused with a different request")
	// ErrInProgress means another request with this key is currently executing.
	ErrInProgress = errors.New("a request with this idempotency key is in progress")
	// ErrLostLease means our lease expired and another worker re-claimed the key.
	ErrLostLease = errors.New("idempotency lease lost")
)

// Response is the stored HTTP outcome of an operation.
type Response struct {
	Code int
	Body []byte
}

// Op performs the operation inside tx. Responses with Code >= 400 are
// recorded (so they replay) but their database effects are rolled back.
// A non-nil error means an infrastructure failure: nothing is recorded.
type Op func(ctx context.Context, tx pgx.Tx) (Response, error)

// Manager coordinates idempotent execution.
type Manager struct {
	Store *store.Store
	Lease time.Duration
}

// New returns a Manager with the given lease (how long an in-progress claim
// is honoured before it is considered abandoned).
func New(s *store.Store, lease time.Duration) *Manager {
	return &Manager{Store: s, Lease: lease}
}

// Fingerprint hashes the parts of a request that define its identity.
func Fingerprint(method, path string, body []byte) []byte {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n", method, path)
	h.Write(body)
	return h.Sum(nil)
}

// Do executes op at most once per key. replayed is true when the response
// comes from storage rather than a fresh execution.
func (m *Manager) Do(ctx context.Context, key string, fingerprint []byte, op Op) (resp Response, replayed bool, err error) {
	token, err := newToken()
	if err != nil {
		return Response{}, false, err
	}
	stored, claimed, err := m.claim(ctx, key, fingerprint, token)
	if err != nil || !claimed {
		return stored, err == nil, err
	}

	err = m.Store.WithTx(ctx, func(tx pgx.Tx) error {
		sp, err := tx.Begin(ctx) // savepoint: lets us discard effects of 4xx outcomes
		if err != nil {
			return err
		}
		r, err := op(ctx, sp)
		if err != nil {
			_ = sp.Rollback(ctx)
			return err
		}
		if r.Code >= 400 {
			if err := sp.Rollback(ctx); err != nil {
				return err
			}
		} else if err := sp.Commit(ctx); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE idempotency_keys
			SET status='completed', response_code=$3, response_body=$4, completed_at=now(), locked_until=NULL
			WHERE key=$1 AND lock_token=$2 AND status='in_progress'`, key, token, r.Code, r.Body)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return ErrLostLease
		}
		resp = r
		return nil
	})
	if err != nil {
		m.release(key, token)
		return Response{}, false, err
	}
	return resp, false, nil
}

func (m *Manager) claim(ctx context.Context, key string, fp []byte, token string) (Response, bool, error) {
	var claimed bool
	err := m.Store.Pool.QueryRow(ctx, `INSERT INTO idempotency_keys (key, request_hash, status, lock_token, locked_until)
		VALUES ($1, $2, 'in_progress', $3, now() + $4::interval)
		ON CONFLICT (key) DO UPDATE SET lock_token = EXCLUDED.lock_token, locked_until = EXCLUDED.locked_until
		WHERE idempotency_keys.status = 'in_progress'
		  AND idempotency_keys.locked_until < now()
		  AND idempotency_keys.request_hash = EXCLUDED.request_hash
		RETURNING true`, key, fp, token, m.Lease.String()).Scan(&claimed)
	if err == nil {
		return Response{}, true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Response{}, false, err
	}
	var (
		hash   []byte
		status string
		code   *int
		body   []byte
	)
	err = m.Store.Pool.QueryRow(ctx, `SELECT request_hash, status, response_code, response_body
		FROM idempotency_keys WHERE key = $1`, key).Scan(&hash, &status, &code, &body)
	if errors.Is(err, pgx.ErrNoRows) {
		// Released between our INSERT and SELECT; let the client retry.
		return Response{}, false, ErrInProgress
	}
	if err != nil {
		return Response{}, false, err
	}
	if !bytes.Equal(hash, fp) {
		return Response{}, false, ErrMismatch
	}
	if status != "completed" || code == nil {
		return Response{}, false, ErrInProgress
	}
	return Response{Code: *code, Body: body}, false, nil
}

func (m *Manager) release(key, token string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = m.Store.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE key=$1 AND lock_token=$2 AND status='in_progress'`, key, token)
}

// Sweep deletes completed keys older than ttl (Stripe keeps keys ~24h).
func (m *Manager) Sweep(ctx context.Context, ttl time.Duration) (int64, error) {
	tag, err := m.Store.Pool.Exec(ctx, `DELETE FROM idempotency_keys WHERE created_at < now() - $1::interval AND status='completed'`, ttl.String())
	return tag.RowsAffected(), err
}

func newToken() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	h := hex.EncodeToString(b[:])
	return h[0:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
