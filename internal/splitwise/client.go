package splitwise

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const baseURL = "https://secure.splitwise.com/api/v3.0"

type Client struct {
	apiKey string
	http   *http.Client
}

func NewClient(apiKey string) *Client {
	return &Client{apiKey: apiKey, http: &http.Client{Timeout: 30 * time.Second}}
}

type User struct {
	ID int64 `json:"id"`
}

type ExpenseUser struct {
	User      User   `json:"user"`
	PaidShare string `json:"paid_share"`
	OwedShare string `json:"owed_share"`
}

type Expense struct {
	ID          int64         `json:"id"`
	Description string        `json:"description"`
	Date        time.Time     `json:"date"`
	CurrencyCode string       `json:"currency_code"`
	Payment     bool          `json:"payment"`
	DeletedAt   *time.Time    `json:"deleted_at"`
	Users       []ExpenseUser `json:"users"`
}

// NetBalance returns the current user's net balance for this expense
// (positive = owed to user, negative = user owes).
func (e Expense) NetBalance(userID int64) (float64, bool) {
	for _, u := range e.Users {
		if u.User.ID != userID {
			continue
		}
		paid, err1 := strconv.ParseFloat(u.PaidShare, 64)
		owed, err2 := strconv.ParseFloat(u.OwedShare, 64)
		if err1 != nil || err2 != nil {
			return 0, false
		}
		net := paid - owed
		if net == 0 {
			return 0, false
		}
		return net, true
	}
	return 0, false
}

func (c *Client) CurrentUser(ctx context.Context) (User, error) {
	var resp struct {
		User User `json:"user"`
	}
	if err := c.get(ctx, "/get_current_user", nil, &resp); err != nil {
		return User{}, err
	}
	return resp.User, nil
}

func (c *Client) Expenses(ctx context.Context, groupID int64, since time.Time) ([]Expense, error) {
	var all []Expense
	offset := 0
	for {
		params := url.Values{
			"group_id":    {strconv.FormatInt(groupID, 10)},
			"dated_after": {since.Format(time.RFC3339)},
			"limit":       {"100"},
			"offset":      {strconv.Itoa(offset)},
		}
		var resp struct {
			Expenses []Expense `json:"expenses"`
		}
		if err := c.get(ctx, "/get_expenses", params, &resp); err != nil {
			return nil, err
		}
		all = append(all, resp.Expenses...)
		if len(resp.Expenses) < 100 {
			break
		}
		offset += len(resp.Expenses)
	}
	return all, nil
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	u := baseURL + path
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("splitwise %s: %s", path, resp.Status)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
