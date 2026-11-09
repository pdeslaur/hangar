package main

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"time"

	"github.com/chainguard-dev/clog"
	"github.com/pdeslaur/oci-splitwiseforynab/internal/splitwise"
	"github.com/pdeslaur/oci-splitwiseforynab/internal/ynab"
	"github.com/sethvargo/go-envconfig"
)

type config struct {
	SplitwiseAPIKey  string `env:"SPLITWISE_API_KEY,required"`
	YNABAPIKey       string `env:"YNAB_API_KEY,required"`
	SplitwiseGroupID int64  `env:"SPLITWISE_GROUP_ID,required"`
	YNABBudgetID     string `env:"YNAB_BUDGET_ID,required"`
	YNABAccountID    string `env:"YNAB_ACCOUNT_ID,required"`
	// SyncDays controls how far back to look for Splitwise expenses.
	// YNAB deduplicates by import_id so re-syncing old expenses is safe.
	SyncDays int    `env:"SYNC_DAYS,default=30"`
	Port     string `env:"PORT,default=8080"`
}

type syncer struct {
	sw        *splitwise.Client
	yn        *ynab.Client
	cfg       *config
}

func (s *syncer) sync(ctx context.Context) error {
	user, err := s.sw.CurrentUser(ctx)
	if err != nil {
		return fmt.Errorf("fetching current user: %w", err)
	}

	since := time.Now().UTC().AddDate(0, 0, -s.cfg.SyncDays)
	expenses, err := s.sw.Expenses(ctx, s.cfg.SplitwiseGroupID, since)
	if err != nil {
		return fmt.Errorf("fetching expenses: %w", err)
	}
	clog.InfoContext(ctx, "fetched expenses", "count", len(expenses))

	var txns []ynab.Transaction
	for _, e := range expenses {
		if e.DeletedAt != nil || e.Payment {
			continue
		}
		net, ok := e.NetBalance(user.ID)
		if !ok {
			continue
		}
		// YNAB milliunits: negative = outflow (expense), positive = inflow (reimbursement).
		// Splitwise net: positive = you paid more than your share (owed to you).
		amount := int64(math.Round(net * 1000))
		importID := fmt.Sprintf("splitwise:%d", e.ID)
		if len(importID) > 36 {
			importID = importID[:36]
		}
		txns = append(txns, ynab.Transaction{
			AccountID: s.cfg.YNABAccountID,
			Date:      e.Date.Format("2006-01-02"),
			Amount:    amount,
			PayeeName: e.Description,
			Memo:      fmt.Sprintf("Splitwise #%d", e.ID),
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

func main() {
	ctx := context.Background()
	cfg := envconfig.MustProcess(ctx, &config{})

	s := &syncer{
		sw:  splitwise.NewClient(cfg.SplitwiseAPIKey),
		yn:  ynab.NewClient(cfg.YNABAPIKey),
		cfg: cfg,
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

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	clog.InfoContext(ctx, "listening", "port", cfg.Port)
	if err := http.ListenAndServe(":"+cfg.Port, nil); err != nil {
		clog.FatalContextf(ctx, "server: %v", err)
	}
}
