package spliit

import (
	"maps"
	"testing"
)

// The cases mirror Spliit's packages/domain/src/totals.test.ts.

func share(id string, shares int64) ParticipantShare {
	var p ParticipantShare
	p.LedgerParticipant.ID = id
	p.Shares = shares
	return p
}

func ptr[T any](v T) *T { return &v }

func evenlyThree(id string, amount int64) Expense {
	return Expense{
		ID:              id,
		Amount:          amount,
		SplitMode:       "EVENLY",
		PaidBySplitMode: "EVENLY",
		PaidByList:      []ParticipantShare{share("u1", 1)},
		PaidFor:         []ParticipantShare{share("u1", 1), share("u2", 1), share("u3", 1)},
	}
}

func sum(m map[string]int64) int64 {
	var s int64
	for _, v := range m {
		s += v
	}
	return s
}

func TestPaidForShares(t *testing.T) {
	for _, tt := range []struct {
		name    string
		expense Expense
		want    map[string]int64
	}{{
		name:    "EVENLY tie-break seeded by id-0",
		expense: evenlyThree("id-0", 100),
		want:    map[string]int64{"u1": 34, "u2": 33, "u3": 33},
	}, {
		name:    "EVENLY tie-break seeded by id-1",
		expense: evenlyThree("id-1", 100),
		want:    map[string]int64{"u1": 33, "u2": 34, "u3": 33},
	}, {
		name:    "EVENLY tie-break seeded by id-2",
		expense: evenlyThree("id-2", 100),
		want:    map[string]int64{"u1": 33, "u2": 33, "u3": 34},
	}, {
		name:    "missing id falls back to seed 0",
		expense: evenlyThree("", 100),
		want:    map[string]int64{"u1": 34, "u2": 33, "u3": 33},
	}, {
		name: "BY_AMOUNT mismatch gives residual to first payer",
		expense: Expense{
			Amount:     100,
			SplitMode:  "BY_AMOUNT",
			PaidByList: []ParticipantShare{share("payer", 1)},
			PaidFor:    []ParticipantShare{share("u1", 30), share("u2", 30)},
		},
		want: map[string]int64{"u1": 30, "u2": 30, "payer": 40},
	}, {
		name: "BY_SHARES fixed units",
		expense: Expense{
			ID:         "e1",
			Amount:     300,
			SplitMode:  "BY_SHARES",
			PaidByList: []ParticipantShare{share("u1", 1)},
			PaidFor:    []ParticipantShare{share("u1", 50), share("u2", 100)},
		},
		want: map[string]int64{"u1": 100, "u2": 200},
	}, {
		name: "BY_PERCENTAGE uses basis points",
		expense: Expense{
			ID:         "e1",
			Amount:     1000,
			SplitMode:  "BY_PERCENTAGE",
			PaidByList: []ParticipantShare{share("u1", 1)},
			PaidFor:    []ParticipantShare{share("u1", 2500), share("u2", 7500)},
		},
		want: map[string]int64{"u1": 250, "u2": 750},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.expense.paidForShares()
			if !maps.Equal(got, tt.want) {
				t.Errorf("paidForShares() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPaidForSharesSums(t *testing.T) {
	for _, tt := range []struct {
		name    string
		expense Expense
	}{{
		name:    "negative amounts truncate toward zero",
		expense: evenlyThree("e1", -101),
	}, {
		name: "ITEMIZED cross-currency treats shares as BY_SHARES weights",
		expense: Expense{
			ID:               "e1",
			Amount:           12,
			OriginalAmount:   ptr[int64](20100),
			OriginalCurrency: ptr("ARS"),
			ConversionRate:   ptr(0.00059),
			SplitMode:        "ITEMIZED",
			PaidByList:       []ParticipantShare{share("alice", 1)},
			PaidFor:          []ParticipantShare{share("alice", 6700), share("bob", 13400)},
		},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sum(tt.expense.paidForShares()); got != tt.expense.Amount {
				t.Errorf("paidForShares() sums to %d, want %d", got, tt.expense.Amount)
			}
		})
	}
}

func TestPaidByShares(t *testing.T) {
	for _, tt := range []struct {
		name    string
		expense Expense
		want    map[string]int64
	}{{
		name: "cross-currency converts then distributes against ledger amount",
		expense: Expense{
			ID:               "e1",
			Amount:           9200,
			OriginalAmount:   ptr[int64](10000),
			OriginalCurrency: ptr("USD"),
			ConversionRate:   ptr(0.92),
			PaidBySplitMode:  "BY_AMOUNT",
			PaidByList:       []ParticipantShare{share("u1", 7000), share("u2", 3000)},
		},
		want: map[string]int64{"u1": 6440, "u2": 2760},
	}, {
		name: "cross-currency fractional remainder",
		expense: Expense{
			ID:               "fx-paidby",
			Amount:           3333,
			OriginalAmount:   ptr[int64](10000),
			OriginalCurrency: ptr("EUR"),
			ConversionRate:   ptr(0.3333),
			PaidBySplitMode:  "BY_AMOUNT",
			PaidByList:       []ParticipantShare{share("u1", 7000), share("u2", 3000)},
		},
		want: map[string]int64{"u1": 2333, "u2": 1000},
	}, {
		name: "BY_AMOUNT mismatch residual goes to first payer",
		expense: Expense{
			Amount:          100,
			PaidBySplitMode: "BY_AMOUNT",
			PaidByList:      []ParticipantShare{share("u1", 40), share("u2", 40)},
		},
		want: map[string]int64{"u1": 60, "u2": 40},
	}, {
		name: "ITEMIZED treated as BY_AMOUNT",
		expense: Expense{
			Amount:          100,
			PaidBySplitMode: "ITEMIZED",
			PaidByList:      []ParticipantShare{share("u1", 60), share("u2", 40)},
		},
		want: map[string]int64{"u1": 60, "u2": 40},
	}} {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.expense.paidByShares()
			if !maps.Equal(got, tt.want) {
				t.Errorf("paidByShares() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNetBalance(t *testing.T) {
	dinner := Expense{
		ID:              "id-0",
		Amount:          3000,
		SplitMode:       "EVENLY",
		PaidBySplitMode: "EVENLY",
		PaidByList:      []ParticipantShare{share("me", 1)},
		PaidFor:         []ParticipantShare{share("me", 1), share("you", 1)},
	}
	settlement := dinner
	settlement.CategoryID = "settlement"

	for _, tt := range []struct {
		name        string
		expense     Expense
		participant string
		want        int64
		wantOK      bool
	}{
		{"payer is owed the other half", dinner, "me", 1500, true},
		{"beneficiary owes their half", dinner, "you", -1500, true},
		{"outsider is skipped", dinner, "them", 0, false},
		{"settlements are skipped", settlement, "me", 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := tt.expense.NetBalance(tt.participant)
			if got != tt.want || ok != tt.wantOK {
				t.Errorf("NetBalance(%q) = %d, %v, want %d, %v", tt.participant, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}
