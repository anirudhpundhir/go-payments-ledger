// Command ledgerd runs the payments ledger HTTP server.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/anirudhpundhir/go-payments-ledger/internal/api"
	"github.com/anirudhpundhir/go-payments-ledger/internal/idempotency"
	"github.com/anirudhpundhir/go-payments-ledger/internal/store"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dsn := getenv("DATABASE_URL", "postgres://postgres:postgres@localhost:5432/ledger?sslmode=disable")
	addr := getenv("ADDR", ":8080")

	st, err := store.Open(ctx, dsn)
	if err != nil {
		return err
	}
	defer st.Close()
	if getenv("LEDGER_ISOLATION", "read_committed") == "serializable" {
		st.Isolation = pgx.Serializable
	}
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	idem := idempotency.New(st, 30*time.Second)
	go sweep(ctx, log, idem)

	srv := &http.Server{
		Addr:              addr,
		Handler:           (&api.Server{Store: st, Idem: idem, Log: log}).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}

// sweep expires idempotency keys after 24h, matching Stripe's retention.
func sweep(ctx context.Context, log *slog.Logger, m *idempotency.Manager) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := m.Sweep(ctx, 24*time.Hour); err != nil {
				log.Warn("idempotency sweep failed", "err", err)
			} else if n > 0 {
				log.Info("idempotency keys swept", "count", n)
			}
		}
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
