package spliit

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

const userAgent = "spliitforynab"

// Client calls Spliit's tRPC API with OAuth access tokens from Auth.
type Client struct {
	baseURL string
	http    *http.Client
}

func NewClient(baseURL string, auth *Auth) *Client {
	return &Client{baseURL: baseURL, http: auth.httpClient()}
}

type Group struct {
	Currency     string `json:"currency"`
	CurrencyCode string `json:"currencyCode"`
}

type GroupInfo struct {
	Group Group `json:"group"`
	// CurrentLedgerParticipantID is the token holder's participant in the
	// group, or empty when they aren't a member.
	CurrentLedgerParticipantID string `json:"currentLedgerParticipantId"`
}

type ParticipantShare struct {
	LedgerParticipant struct {
		ID string `json:"id"`
	} `json:"ledgerParticipant"`
	Shares int64 `json:"shares"`
}

type Expense struct {
	ID               string             `json:"id"`
	Title            string             `json:"title"`
	Amount           int64              `json:"amount"` // ledger minor units
	ExpenseDate      time.Time          `json:"expenseDate"`
	ExpenseTimeZone  string             `json:"expenseTimeZone"`
	CategoryID       string             `json:"categoryId"`
	SplitMode        string             `json:"splitMode"`
	PaidBySplitMode  string             `json:"paidBySplitMode"`
	OriginalAmount   *int64             `json:"originalAmount"`
	OriginalCurrency *string            `json:"originalCurrency"`
	ConversionRate   *float64           `json:"conversionRate"`
	ConversionSource *string            `json:"conversionSource"`
	PaidByList       []ParticipantShare `json:"paidByList"`
	PaidFor          []ParticipantShare `json:"paidFor"`
}

// LocalDate returns the expense's date in the time zone it was entered in.
func (e Expense) LocalDate() string {
	loc, err := time.LoadLocation(e.ExpenseTimeZone)
	if err != nil {
		loc = time.UTC
	}
	return e.ExpenseDate.In(loc).Format("2006-01-02")
}

func (c *Client) Group(ctx context.Context, groupID string) (GroupInfo, error) {
	var out GroupInfo
	err := c.query(ctx, "groups.get", map[string]any{"groupId": groupID}, nil, &out)
	return out, err
}

func (c *Client) Expenses(ctx context.Context, groupID string, since time.Time) ([]Expense, error) {
	const limit = 100
	var all []Expense
	cursor := 0
	for {
		input := map[string]any{
			"groupId":  groupID,
			"cursor":   cursor,
			"limit":    limit,
			"dateFrom": since.Format(time.RFC3339),
		}
		// superjson needs to be told dateFrom is a Date.
		meta := map[string]any{"values": map[string][]string{"dateFrom": {"Date"}}}
		var resp struct {
			Expenses []Expense `json:"expenses"`
			HasMore  bool      `json:"hasMore"`
		}
		if err := c.query(ctx, "groups.expenses.list", input, meta, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Expenses...)
		if !resp.HasMore {
			break
		}
		cursor += limit
	}
	return all, nil
}

// query calls a tRPC query procedure, wrapping input and unwrapping the result
// in superjson envelopes.
func (c *Client) query(ctx context.Context, procedure string, input, meta, out any) error {
	envelope := map[string]any{"json": input}
	if meta != nil {
		envelope["meta"] = meta
	}
	b, err := json.Marshal(envelope)
	if err != nil {
		return err
	}
	u := c.baseURL + "/trpc/" + procedure + "?" + url.Values{"input": {string(b)}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	var body struct {
		Result struct {
			Data struct {
				JSON json.RawMessage `json:"json"`
			} `json:"data"`
		} `json:"result"`
		Error struct {
			JSON struct {
				Message string `json:"message"`
			} `json:"json"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return fmt.Errorf("spliit %s: %s: decoding response: %w", procedure, resp.Status, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spliit %s: %s: %s", procedure, resp.Status, body.Error.JSON.Message)
	}
	return json.Unmarshal(body.Result.Data.JSON, out)
}
