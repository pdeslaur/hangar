package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"time"
	_ "time/tzdata" // expense dates are local to their time zone

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/oci-spliitforynab/internal/spliit"
	"github.com/pdeslaur/oci-spliitforynab/internal/ynab"
	"github.com/sethvargo/go-envconfig"
)

type config struct {
	// SpliitURL is the Spliit web origin, e.g. https://spliit.example.com.
	SpliitURL     string `env:"SPLIIT_URL,required"`
	SpliitGroupID string `env:"SPLIIT_GROUP_ID,required"`
	YNABAPIKey    string `env:"YNAB_API_KEY,required"`
	YNABBudgetID  string `env:"YNAB_BUDGET_ID,required"`
	YNABAccountID string `env:"YNAB_ACCOUNT_ID,required"`
	// StateDir holds the Spliit OAuth grant, which rotates on every refresh.
	StateDir string `env:"STATE_DIR,default=/data"`
	// PublicURL is where the browser reaches this service during /login.
	PublicURL string `env:"PUBLIC_URL,default=http://127.0.0.1:8080"`
	// SyncDays controls how far back to look for Spliit expenses.
	// YNAB deduplicates by import_id so re-syncing old expenses is safe.
	SyncDays     int           `env:"SYNC_DAYS,default=30"`
	SyncInterval time.Duration `env:"SYNC_INTERVAL,default=0"`
	// ListenAddr overrides :PORT when set (e.g. 127.0.0.1:8080 for homelab).
	Port       string `env:"PORT,default=8080"`
	ListenAddr string `env:"LISTEN_ADDR"`
}

func (c *config) listenAddr() string {
	if c.ListenAddr != "" {
		return c.ListenAddr
	}
	return ":" + c.Port
}

type syncer struct {
	sp  *spliit.Client
	yn  *ynab.Client
	cfg *config
}

func (s *syncer) sync(ctx context.Context) error {
	info, err := s.sp.Group(ctx, s.cfg.SpliitGroupID)
	if err != nil {
		return fmt.Errorf("fetching group: %w", err)
	}
	if info.CurrentLedgerParticipantID == "" {
		return errors.New("signed-in Spliit account isn't a participant in the group")
	}
	toMilliunits, err := milliunitsPerMinorUnit(info.Group.CurrencyCode)
	if err != nil {
		return err
	}

	since := time.Now().UTC().AddDate(0, 0, -s.cfg.SyncDays)
	expenses, err := s.sp.Expenses(ctx, s.cfg.SpliitGroupID, since)
	if err != nil {
		return fmt.Errorf("fetching expenses: %w", err)
	}
	clog.InfoContext(ctx, "fetched expenses", "count", len(expenses))

	var txns []ynab.Transaction
	for _, e := range expenses {
		net, ok := e.NetBalance(info.CurrentLedgerParticipantID)
		if !ok {
			continue
		}
		// YNAB milliunits: negative = outflow (expense), positive = inflow (reimbursement).
		// Spliit net: positive = you paid more than your share (owed to you).
		importID := "spliit:" + e.ID
		if len(importID) > 36 {
			importID = importID[:36]
		}
		txns = append(txns, ynab.Transaction{
			AccountID: s.cfg.YNABAccountID,
			Date:      e.LocalDate(),
			Amount:    net * toMilliunits,
			PayeeName: e.Title,
			Memo:      "Spliit " + e.ID,
			ImportID:  importID,
			Cleared:   "cleared",
		})
	}

	if err := s.yn.ImportTransactions(ctx, s.cfg.YNABBudgetID, txns); err != nil {
		return fmt.Errorf("importing to YNAB: %w", err)
	}
	clog.InfoContext(ctx, "sync complete", "imported", len(txns))
	return nil
}

// milliunitsPerMinorUnit converts Spliit amounts, in the currency's minor
// units, to YNAB milliunits. YNAB can't represent more than three decimals.
func milliunitsPerMinorUnit(currencyCode string) (int64, error) {
	switch currencyCode {
	case "CLP", "COP", "CRC", "HUF", "IDR", "IQD", "IRR", "ISK", "JPY", "KRW", "LAK", "LBP",
		"MMK", "MUR", "PKR", "PYG", "RSD", "TZS", "UGX", "UZS", "VND", "XAF", "XOF":
		return 1000, nil
	case "BHD", "JOD", "KWD", "OMR", "TND":
		return 1, nil
	case "SAT", "XRP", "ETH", "LTC", "SOL", "BTC":
		return 0, fmt.Errorf("group currency %s isn't supported by YNAB", currencyCode)
	}
	return 10, nil
}

// run calls fn immediately, then calls it again on each interval tick until ctx
// is cancelled. Errors from fn are logged but do not stop the loop.
func run(ctx context.Context, interval time.Duration, fn func(context.Context) error) {
	if err := fn(ctx); err != nil {
		clog.ErrorContext(ctx, "sync failed", "err", err)
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := fn(ctx); err != nil {
				clog.ErrorContext(ctx, "sync failed", "err", err)
			}
		}
	}
}

func main() {
	ctx := context.Background()
	cfg := envconfig.MustProcess(ctx, &config{})

	auth, err := spliit.NewAuth(cfg.SpliitURL, cfg.PublicURL+"/callback", filepath.Join(cfg.StateDir, "token.json"))
	if err != nil {
		clog.FatalContextf(ctx, "loading Spliit token: %v", err)
	}

	s := &syncer{
		sp:  spliit.NewClient(cfg.SpliitURL, auth),
		yn:  ynab.NewClient(cfg.YNABAPIKey),
		cfg: cfg,
	}

	if cfg.SyncInterval > 0 {
		go run(ctx, cfg.SyncInterval, s.sync)
	}

	http.HandleFunc("/sync", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if err := s.sync(r.Context()); err != nil {
			clog.ErrorContext(r.Context(), "sync failed", "err", err)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	http.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		u, err := auth.LoginURL(r.Context())
		if err != nil {
			clog.ErrorContext(r.Context(), "login failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		http.Redirect(w, r, u, http.StatusFound)
	})

	http.HandleFunc("GET /callback", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		if e := q.Get("error"); e != "" {
			http.Error(w, "Spliit sign-in failed: "+e+": "+q.Get("error_description"), http.StatusBadRequest)
			return
		}
		if err := auth.Callback(r.Context(), q.Get("state"), q.Get("code")); err != nil {
			clog.ErrorContext(r.Context(), "login callback failed", "err", err)
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clog.InfoContext(r.Context(), "signed in to Spliit")
		fmt.Fprintln(w, "Signed in to Spliit. Syncs will run with this account.")
	})

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := cfg.listenAddr()
	clog.InfoContext(ctx, "listening", "addr", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		clog.FatalContextf(ctx, "server: %v", err)
	}
}
