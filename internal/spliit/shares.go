package spliit

import (
	"math"
	"math/big"
	"sort"
	"unicode/utf16"
)

// The functions in this file port Spliit's split math from
// packages/domain/src/totals.ts and remainder-distribution.ts, so that the
// per-participant amounts match what the Spliit UI shows to the cent.

const settlementCategoryID = "settlement"

// NetBalance returns the participant's net balance for this expense in ledger
// minor units (positive = owed to the participant, negative = participant
// owes). Settlements and expenses the participant isn't part of are reported
// as not ok.
func (e Expense) NetBalance(participantID string) (int64, bool) {
	if e.CategoryID == settlementCategoryID {
		return 0, false
	}
	net := e.paidByShares()[participantID] - e.paidForShares()[participantID]
	if net == 0 {
		return 0, false
	}
	return net, true
}

type exactShare struct {
	id     string
	amount *big.Rat
}

// isConverted reports whether the expense was entered in another currency.
func (e Expense) isConverted() bool {
	if e.ConversionSource != nil {
		switch *e.ConversionSource {
		case "EXCHANGE", "CUSTOM", "EXACT":
			return true
		}
	}
	return e.OriginalCurrency != nil && *e.OriginalCurrency != "" && e.hasRate()
}

func (e Expense) hasRate() bool {
	return e.ConversionRate != nil && *e.ConversionRate != 0
}

func (e Expense) originalAmount() int64 {
	if e.OriginalAmount != nil {
		return *e.OriginalAmount
	}
	return e.Amount
}

// conversionScale converts original-currency minor units to ledger minor units.
func (e Expense) conversionScale() float64 {
	if orig := e.originalAmount(); orig != 0 {
		return float64(e.Amount) / float64(orig)
	}
	return *e.ConversionRate
}

// paidForShares returns how much of the expense each participant owes, in
// ledger minor units; the values sum to the expense amount.
func (e Expense) paidForShares() map[string]int64 {
	if len(e.PaidFor) == 0 {
		return map[string]int64{}
	}
	converted, hasRate := e.isConverted(), e.hasRate()

	// BY_AMOUNT cross-currency shares are original-currency minor units, and
	// ITEMIZED cross-currency shares are weights against the ledger total.
	mode := e.SplitMode
	if mode == "ITEMIZED" && converted && hasRate {
		mode = "BY_SHARES"
	}
	sharesAmount := e.Amount
	scaleShares := converted && hasRate && e.SplitMode == "BY_AMOUNT"
	if scaleShares {
		sharesAmount = e.originalAmount()
	}

	exact := exactShares(sharesAmount, mode, e.PaidFor)
	if scaleShares {
		exact = convertByRate(exact, e.conversionScale())
	}

	payerID := ""
	if !converted && (e.SplitMode == "BY_AMOUNT" || e.SplitMode == "ITEMIZED") && len(e.PaidByList) > 0 {
		payerID = e.PaidByList[0].LedgerParticipant.ID
	}
	return distributeRemainder(exact, e.Amount, seedFromID(e.ID), payerID)
}

// paidByShares returns how much of the expense each participant paid, in
// ledger minor units; the values sum to the expense amount.
func (e Expense) paidByShares() map[string]int64 {
	if len(e.PaidByList) == 0 {
		return map[string]int64{}
	}
	converted, hasRate := e.isConverted(), e.hasRate()

	payerBase := e.Amount
	if converted {
		payerBase = e.originalAmount()
	}
	mode := e.PaidBySplitMode
	if mode == "ITEMIZED" {
		mode = "BY_AMOUNT"
	}

	exact := exactShares(payerBase, mode, e.PaidByList)
	if converted && hasRate {
		exact = convertByRate(exact, e.conversionScale())
	}

	payerID := ""
	if !converted && (e.PaidBySplitMode == "BY_AMOUNT" || e.PaidBySplitMode == "ITEMIZED") {
		payerID = e.PaidByList[0].LedgerParticipant.ID
	}
	return distributeRemainder(exact, e.Amount, seedFromID(e.ID), payerID)
}

// exactShares splits amount among participants as exact rationals. The
// meaning of each participant's stored shares depends on the split mode.
func exactShares(amount int64, mode string, participants []ParticipantShare) []exactShare {
	var out []exactShare
	index := map[string]int{}
	add := func(id string, r *big.Rat) {
		if i, ok := index[id]; ok {
			out[i].amount.Add(out[i].amount, r)
			return
		}
		index[id] = len(out)
		out = append(out, exactShare{id: id, amount: r})
	}

	if amount == 0 {
		for _, p := range participants {
			add(p.LedgerParticipant.ID, new(big.Rat))
		}
		return out
	}

	switch mode {
	case "EVENLY":
		for _, p := range participants {
			add(p.LedgerParticipant.ID, big.NewRat(amount, int64(len(participants))))
		}
	case "BY_SHARES":
		var total int64
		for _, p := range participants {
			total += p.Shares
		}
		for _, p := range participants {
			if total == 0 {
				add(p.LedgerParticipant.ID, new(big.Rat))
				continue
			}
			num := new(big.Int).Mul(big.NewInt(amount), big.NewInt(p.Shares))
			add(p.LedgerParticipant.ID, new(big.Rat).SetFrac(num, big.NewInt(total)))
		}
	case "BY_PERCENTAGE":
		// Shares are basis points.
		for _, p := range participants {
			num := new(big.Int).Mul(big.NewInt(amount), big.NewInt(p.Shares))
			add(p.LedgerParticipant.ID, new(big.Rat).SetFrac(num, big.NewInt(10000)))
		}
	case "BY_AMOUNT", "ITEMIZED":
		// Shares are minor units.
		for _, p := range participants {
			add(p.LedgerParticipant.ID, new(big.Rat).SetInt64(p.Shares))
		}
	}
	return out
}

// convertByRate scales each share and rounds it to an integer the way
// JavaScript's Math.round does.
func convertByRate(shares []exactShare, rate float64) []exactShare {
	out := make([]exactShare, len(shares))
	for i, s := range shares {
		num, _ := new(big.Float).SetInt(s.amount.Num()).Float64()
		den, _ := new(big.Float).SetInt(s.amount.Denom()).Float64()
		rounded := math.Floor(num/den*rate + 0.5)
		out[i] = exactShare{id: s.id, amount: new(big.Rat).SetInt64(int64(rounded))}
	}
	return out
}

// distributeRemainder truncates each share toward zero, then hands out the
// leftover minor units: all to payerID when set, otherwise one at a time by
// descending fractional part, with ties rotated by seed.
func distributeRemainder(shares []exactShare, amount int64, seed uint32, payerID string) map[string]int64 {
	result := map[string]int64{}
	if len(shares) == 0 {
		if payerID != "" && amount != 0 {
			result[payerID] = amount
		}
		return result
	}

	type entry struct {
		id   string
		frac *big.Rat
	}
	entries := make([]entry, 0, len(shares))
	var sum int64
	for _, s := range shares {
		truncated := new(big.Int).Quo(s.amount.Num(), s.amount.Denom())
		result[s.id] = truncated.Int64()
		sum += truncated.Int64()
		frac := new(big.Rat).Sub(s.amount, new(big.Rat).SetInt(truncated))
		entries = append(entries, entry{id: s.id, frac: frac.Abs(frac)})
	}

	diff := amount - sum
	if diff == 0 {
		return result
	}
	if payerID != "" {
		result[payerID] += diff
		return result
	}

	sort.SliceStable(entries, func(i, j int) bool {
		if c := entries[i].frac.Cmp(entries[j].frac); c != 0 {
			return c > 0
		}
		return entries[i].id < entries[j].id
	})

	// Rotate within runs of equal fractions so the seed breaks ties fairly
	// across expenses.
	var order []string
	for i := 0; i < len(entries); {
		j := i + 1
		for j < len(entries) && entries[j].frac.Cmp(entries[i].frac) == 0 {
			j++
		}
		group := entries[i:j]
		offset := int(seed % uint32(len(group)))
		for k := range group {
			order = append(order, group[(k+offset)%len(group)].id)
		}
		i = j
	}

	step := int64(1)
	if diff < 0 {
		step = -1
		diff = -diff
	}
	for i := int64(0); i < diff; i++ {
		result[order[i%int64(len(order))]] += step
	}
	return result
}

// seedFromID hashes an expense ID with 32-bit FNV-1a over UTF-16 code units,
// matching Spliit's hashStringToSeed.
func seedFromID(id string) uint32 {
	if id == "" {
		return 0
	}
	hash := uint32(0x811c9dc5)
	for _, c := range utf16.Encode([]rune(id)) {
		hash ^= uint32(c)
		hash *= 0x01000193
	}
	return hash
}
