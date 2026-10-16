// Package api exposes the ledger over an HTTP JSON API.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/anirudhpundhir/go-payments-ledger/internal/idempotency"
	"github.com/anirudhpundhir/go-payments-ledger/internal/ledger"
	"github.com/anirudhpundhir/go-payments-ledger/internal/store"
)

const (
	maxBodyBytes   = 64 << 10
	maxKeyLength   = 255
	idempotencyHdr = "Idempotency-Key"
	replayedHdr    = "Idempotent-Replayed"
)

var uuidRE = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// Server wires HTTP handlers to the store and idempotency manager.
type Server struct {
	Store *store.Store
	Idem  *idempotency.Manager
	Log   *slog.Logger
}

// Handler returns the routed http.Handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.Store.Pool.Ping(r.Context()); err != nil {
			writeError(w, http.StatusServiceUnavailable, "unavailable", err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /v1/accounts", s.mutating(s.createAccount))
	mux.HandleFunc("POST /v1/transfers", s.mutating(s.createTransfer))
	mux.HandleFunc("GET /v1/accounts/{id}", s.getAccount)
	mux.HandleFunc("GET /v1/accounts/{id}/balance", s.getBalance)
	mux.HandleFunc("GET /v1/accounts/{id}/entries", s.listEntries)
	return mux
}

// opBuilder validates a request body and returns the transactional operation.
// Validation errors are returned as an immediate Response (never stored).
type opBuilder func(body []byte) (idempotency.Op, *idempotency.Response)

// mutating handles body reading, the Idempotency-Key protocol and retries.
func (s *Server) mutating(build opBuilder) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
		if err != nil {
			writeError(w, http.StatusRequestEntityTooLarge, "invalid_request", "body too large or unreadable")
			return
		}
		key := r.Header.Get(idempotencyHdr)
		if len(key) > maxKeyLength {
			writeError(w, http.StatusBadRequest, "invalid_request", "Idempotency-Key too long")
			return
		}
		op, early := build(body)
		if early != nil {
			writeRaw(w, *early)
			return
		}
		if key == "" {
			var resp idempotency.Response
			err := s.Store.WithTx(r.Context(), func(tx pgx.Tx) error {
				var err error
				resp, err = op(r.Context(), tx)
				if err == nil && resp.Code >= 400 {
					return errRollback
				}
				return err
			})
			if err != nil && !errors.Is(err, errRollback) {
				s.internal(w, err)
				return
			}
			writeRaw(w, resp)
			return
		}
		fp := idempotency.Fingerprint(r.Method, r.URL.Path, body)
		resp, replayed, err := s.Idem.Do(r.Context(), key, fp, op)
		switch {
		case errors.Is(err, idempotency.ErrMismatch):
			writeError(w, http.StatusUnprocessableEntity, "idempotency_error",
				"Keys for idempotent requests can only be used with the same parameters they were first used with.")
			return
		case errors.Is(err, idempotency.ErrInProgress):
			writeError(w, http.StatusConflict, "idempotency_error",
				"There is currently another in-progress request using this Idempotency-Key. Retry later.")
			return
		case err != nil:
			s.internal(w, err)
			return
		}
		if replayed {
			w.Header().Set(replayedHdr, "true")
		}
		writeRaw(w, resp)
	}
}

var errRollback = errors.New("rollback")

type createAccountReq struct {
	Name          string `json:"name"`
	Currency      string `json:"currency"`
	AllowNegative bool   `json:"allow_negative"`
}

func (s *Server) createAccount(body []byte) (idempotency.Op, *idempotency.Response) {
	var req createAccountReq
	if err := decodeStrict(body, &req); err != nil {
		return nil, errResp(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if req.Name == "" {
		return nil, errResp(http.StatusBadRequest, "invalid_request", "name is required")
	}
	if err := ledger.ValidateCurrency(req.Currency); err != nil {
		return nil, errResp(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		a, err := store.CreateAccount(ctx, tx, req.Name, req.Currency, req.AllowNegative)
		if err != nil {
			return idempotency.Response{}, err
		}
		return jsonResp(http.StatusCreated, a), nil
	}, nil
}

type transferReq struct {
	FromAccountID string `json:"from_account_id"`
	ToAccountID   string `json:"to_account_id"`
	Amount        int64  `json:"amount"`
	Currency      string `json:"currency"`
	Description   string `json:"description"`
}

func (s *Server) createTransfer(body []byte) (idempotency.Op, *idempotency.Response) {
	var req transferReq
	if err := decodeStrict(body, &req); err != nil {
		return nil, errResp(http.StatusBadRequest, "invalid_request", err.Error())
	}
	if !uuidRE.MatchString(req.FromAccountID) || !uuidRE.MatchString(req.ToAccountID) {
		return nil, errResp(http.StatusBadRequest, "invalid_request", "account ids must be UUIDs")
	}
	ps, err := ledger.TransferPostings(req.FromAccountID, req.ToAccountID, req.Amount, req.Currency)
	if err != nil {
		return nil, errResp(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return func(ctx context.Context, tx pgx.Tx) (idempotency.Response, error) {
		e, err := store.PostEntry(ctx, tx, req.Description, ps)
		if r, ok := domainErr(err); ok {
			return *r, nil
		}
		if err != nil {
			return idempotency.Response{}, err
		}
		return jsonResp(http.StatusCreated, e), nil
	}, nil
}

// domainErr maps business-rule failures to stored 4xx responses.
func domainErr(err error) (*idempotency.Response, bool) {
	switch {
	case err == nil:
		return nil, false
	case errors.Is(err, ledger.ErrAccountNotFound):
		return errResp(http.StatusNotFound, "not_found", err.Error()), true
	case errors.Is(err, ledger.ErrInsufficientFunds):
		return errResp(http.StatusUnprocessableEntity, "insufficient_funds", err.Error()), true
	case errors.Is(err, ledger.ErrCurrencyMismatch):
		return errResp(http.StatusUnprocessableEntity, "currency_mismatch", err.Error()), true
	case errors.Is(err, ledger.ErrInvalidAmount), errors.Is(err, ledger.ErrUnbalanced):
		return errResp(http.StatusUnprocessableEntity, "invalid_request", err.Error()), true
	}
	return nil, false
}

func (s *Server) getAccount(w http.ResponseWriter, r *http.Request) {
	a, ok := s.loadAccount(w, r)
	if ok {
		writeJSON(w, http.StatusOK, a)
	}
}

func (s *Server) getBalance(w http.ResponseWriter, r *http.Request) {
	a, ok := s.loadAccount(w, r)
	if ok {
		writeJSON(w, http.StatusOK, map[string]any{"account_id": a.ID, "balance": a.Balance, "currency": a.Currency})
	}
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidRE.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", ledger.ErrAccountNotFound.Error())
		return
	}
	limit, before := 50, int64(0)
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 500 {
			writeError(w, http.StatusBadRequest, "invalid_request", "limit must be 1..500")
			return
		}
		limit = n
	}
	if v := r.URL.Query().Get("before"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 1 {
			writeError(w, http.StatusBadRequest, "invalid_request", "before must be a posting id")
			return
		}
		before = n
	}
	lines, err := s.Store.ListEntries(r.Context(), id, before, limit)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	if err != nil {
		s.internal(w, err)
		return
	}
	out := map[string]any{"data": lines, "has_more": len(lines) == limit}
	if len(lines) == limit {
		out["next_before"] = lines[len(lines)-1].PostingID
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) loadAccount(w http.ResponseWriter, r *http.Request) (ledger.Account, bool) {
	id := r.PathValue("id")
	if !uuidRE.MatchString(id) {
		writeError(w, http.StatusNotFound, "not_found", ledger.ErrAccountNotFound.Error())
		return ledger.Account{}, false
	}
	a, err := s.Store.GetAccount(r.Context(), id)
	if errors.Is(err, ledger.ErrAccountNotFound) {
		writeError(w, http.StatusNotFound, "not_found", err.Error())
		return a, false
	}
	if err != nil {
		s.internal(w, err)
		return a, false
	}
	return a, true
}

func (s *Server) internal(w http.ResponseWriter, err error) {
	if s.Log != nil {
		s.Log.Error("internal error", "err", err)
	}
	writeError(w, http.StatusInternalServerError, "api_error", "internal error")
}

func decodeStrict(body []byte, v any) error {
	if len(body) == 0 {
		return errors.New("request body is required")
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected trailing data")
	}
	return nil
}

type errBody struct {
	Error struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	} `json:"error"`
}

func errResp(code int, typ, msg string) *idempotency.Response {
	var b errBody
	b.Error.Type, b.Error.Message = typ, msg
	r := jsonResp(code, b)
	return &r
}

func jsonResp(code int, v any) idempotency.Response {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only marshals our own types
	}
	return idempotency.Response{Code: code, Body: b}
}

func writeRaw(w http.ResponseWriter, r idempotency.Response) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(r.Code)
	_, _ = w.Write(r.Body)
}

func writeJSON(w http.ResponseWriter, code int, v any) { writeRaw(w, jsonResp(code, v)) }

func writeError(w http.ResponseWriter, code int, typ, msg string) {
	writeRaw(w, *errResp(code, typ, msg))
}
