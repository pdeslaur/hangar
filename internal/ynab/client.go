package ynab

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

const baseURL = "https://api.ynab.com/v1"

type Client struct {
	apiKey string
	http   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

type Transaction struct {
	AccountID  string `json:"account_id"`
	Date       string `json:"date"` // YYYY-MM-DD
	Amount     int64  `json:"amount"` // milliunits; negative = outflow
	PayeeName  string `json:"payee_name,omitempty"`
	Memo       string `json:"memo,omitempty"`
	ImportID   string `json:"import_id,omitempty"` // max 36 chars; deduplicates on import
	Cleared    string `json:"cleared"`
}

// ImportTransactions creates or deduplicates transactions in a YNAB budget.
// YNAB will silently skip any transaction whose import_id already exists.
func (c *Client) ImportTransactions(ctx context.Context, budgetID string, txns []Transaction) error {
	if len(txns) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"transactions": txns})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		baseURL+"/budgets/"+budgetID+"/transactions", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		var e struct {
			Error struct {
				Detail string `json:"detail"`
			} `json:"error"`
		}
		json.NewDecoder(resp.Body).Decode(&e) //nolint:errcheck
		return fmt.Errorf("ynab import: %s: %s", resp.Status, e.Error.Detail)
	}
	return nil
}
