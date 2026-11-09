package agent

import (
	"math"
	"sync"

	"github.com/0xyu/aco/internal/config"
)

// Budget tracks USD spending of api-mode invocations per scope against
// limits.budget_usd (§14.3, §16.5). Subscription-mode usage is never charged.
// A limit of 0 means unlimited. Safe for concurrent use.
type Budget struct {
	mu        sync.Mutex
	limits    map[Scope]float64
	spent     map[Scope]float64
	softRatio float64
}

// NewBudget returns a budget with the configured limits.
func NewBudget(l config.BudgetLimits) *Budget {
	return &Budget{
		limits: map[Scope]float64{
			ScopeRun: l.Run, ScopePlanning: l.Planning, ScopePhase: l.Phase, ScopeFinal: l.Final,
		},
		spent:     map[Scope]float64{},
		softRatio: l.SoftRatio,
	}
}

// BudgetStatus is the state after a charge.
type BudgetStatus struct {
	Warn     []Scope // crossed soft_ratio × limit
	Exceeded []Scope // reached the limit
}

// Remaining is the smallest amount left over the given scopes (+Inf when
// none of them has a limit).
func (b *Budget) Remaining(scopes ...Scope) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	rem := math.Inf(1)
	for _, s := range scopes {
		if lim := b.limits[s]; lim > 0 {
			rem = math.Min(rem, lim-b.spent[s])
		}
	}
	return rem
}

// Spent returns the amount charged to scope.
func (b *Budget) Spent(s Scope) float64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.spent[s]
}

// ResetScope clears the spending of a per-unit scope (a new phase).
func (b *Budget) ResetScope(s Scope) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.spent, s)
}

// Charge adds usd to every scope and reports which crossed their soft or
// hard limit.
func (b *Budget) Charge(usd float64, scopes ...Scope) BudgetStatus {
	b.mu.Lock()
	defer b.mu.Unlock()
	var st BudgetStatus
	if usd < 0 || math.IsNaN(usd) || math.IsInf(usd, 0) {
		usd = 0
	}
	for _, s := range scopes {
		b.spent[s] += usd
		lim := b.limits[s]
		if lim <= 0 {
			continue
		}
		switch {
		case b.spent[s] >= lim:
			st.Exceeded = append(st.Exceeded, s)
		case b.softRatio > 0 && b.spent[s] >= lim*b.softRatio:
			st.Warn = append(st.Warn, s)
		}
	}
	return st
}
