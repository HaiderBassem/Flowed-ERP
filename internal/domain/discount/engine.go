// Package discount implements the discount subsystem: definitions, grants, and
// the engine that turns them into frozen amounts on an account.
package discount

import (
	"sort"

	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/shared"
)

// ValueType distinguishes a rate from a flat sum.
type ValueType string

const (
	// ValuePercentage is a rate in basis points applied to the discountable base.
	ValuePercentage ValueType = "percentage"
	// ValueFixed is a flat number of dinars.
	ValueFixed ValueType = "fixed"
)

// TruncationReason records why an application delivered less than it computed.
// Nothing is ever silently reduced: if a cap or a floor bit, the row says so.
type TruncationReason string

const (
	// TruncatedByPerApplicationCap means the definition's own ceiling applied.
	TruncatedByPerApplicationCap TruncationReason = "per_application_cap"
	// TruncatedByTotalCap means the year's maximum total discount applied.
	TruncatedByTotalCap TruncationReason = "total_cap"
	// TruncatedByDiscountableFloor means the discountable base ran out.
	TruncatedByDiscountableFloor TruncationReason = "discountable_floor"
)

// Candidate is one grant offered to the engine, flattened from a definition
// version and the assignment that awarded it.
type Candidate struct {
	AssignmentID        shared.ID
	DefinitionID        shared.ID
	DefinitionVersionID shared.ID
	DefinitionCode      string
	ValueType           ValueType
	Rate                money.BasisPoints
	FixedAmount         money.Amount
	// AppliesToComponents narrows the base to specific fee components. Empty
	// means every discountable component, which is what lets a full exemption
	// clear tuition while leaving the identity-card charge payable.
	AppliesToComponents []string
	PerApplicationCap   *money.Amount
	Stackable           bool
	ExclusivityGroupID  *shared.ID
	Priority            int16
	IsFullExemption     bool
}

// ComponentAmount is one line of the frozen fee snapshot the engine works from.
type ComponentAmount struct {
	Code         string
	Amount       money.Amount
	Discountable bool
}

// Result is one computed application, ready to be persisted as a frozen row.
type Result struct {
	Candidate        Candidate
	FrozenBase       money.Amount
	ComputedAmount   money.Amount
	AppliedAmount    money.Amount
	TruncationReason *TruncationReason
	Sequence         int16
}

// Truncated reports whether the applied amount fell short of the computed one.
func (r Result) Truncated() bool { return r.AppliedAmount < r.ComputedAmount }

// Computation is the engine's full output for one account.
type Computation struct {
	GrossTotal       money.Amount
	DiscountableBase money.Amount
	Applications     []Result
	TotalDiscount    money.Amount
	NetTotal         money.Amount
}

// Input is everything the engine needs. It takes no repositories and reads no
// clock: given the same components, candidates and cap it returns the same
// numbers forever, which is what makes a recomputation years later match the
// receipt the student is holding.
type Input struct {
	Components []ComponentAmount
	Candidates []Candidate
	// MaxTotalDiscountBP caps the sum of all discounts as a rate of the
	// discountable base. It comes from the fee policy, so it freezes with the
	// year rather than following a definition that someone edits later.
	MaxTotalDiscountBP money.BasisPoints
}

// Compute applies every candidate and returns the frozen result.
//
// The rules, in the order they matter:
//
//  1. Percentages are computed against the ORIGINAL discountable base, never
//     against the running remainder. Sequential application would make the
//     outcome depend on evaluation order — a ten-percent grant plus a fixed
//     100,000 grant gives 1,700,000 one way and 1,710,000 the other — and no
//     cashier or ministry auditor can be asked to accept that. It also matches
//     how Iraqi circulars phrase a discount: fifteen percent of the tuition,
//     not fifteen percent of whatever is left.
//
//  2. Percentages run before fixed amounts, each class ordered by priority
//     then sequence, so a truncating cap always bites in a defined place.
//
//  3. The floor is the non-discountable remainder, not zero. An account of
//     300,000 tuition plus 200,000 in registration and card fees cannot be
//     reduced below 200,000 by any combination of grants, because those
//     charges are not discountable and the university still collects them.
//
//  4. Nothing disappears quietly. A grant clipped by a cap keeps its computed
//     amount alongside the applied one and names the reason.
func Compute(in Input) (*Computation, error) {
	gross, discountableBase, err := totals(in.Components)
	if err != nil {
		return nil, err
	}

	candidates, err := screenExclusivity(in.Candidates)
	if err != nil {
		return nil, err
	}
	sortCandidates(candidates)

	capLimit := discountableBase
	if in.MaxTotalDiscountBP > 0 && in.MaxTotalDiscountBP < money.FullRate {
		limit, err := money.ApplyRate(discountableBase, in.MaxTotalDiscountBP)
		if err != nil {
			return nil, shared.Internal("discount.cap_computation_failed", err,
				"computing the maximum total discount")
		}
		capLimit = limit
	}

	results := make([]Result, 0, len(candidates))
	var runningTotal money.Amount

	for i, c := range candidates {
		base, err := baseFor(c, in.Components, discountableBase)
		if err != nil {
			return nil, err
		}

		computed, err := computeRaw(c, base)
		if err != nil {
			return nil, err
		}

		applied := computed
		var reason *TruncationReason

		if c.PerApplicationCap != nil && applied > *c.PerApplicationCap {
			applied = *c.PerApplicationCap
			r := TruncatedByPerApplicationCap
			reason = &r
		}

		// The policy cap and the discountable floor are the same arithmetic
		// seen from two directions; whichever binds first names itself.
		remainingUnderCap, err := capLimit.Sub(runningTotal)
		if err != nil {
			return nil, shared.Internal("discount.cap_arithmetic_failed", err, "applying the total discount cap")
		}
		remainingUnderCap = remainingUnderCap.ClampNonNegative()

		if applied > remainingUnderCap {
			applied = remainingUnderCap
			r := TruncatedByTotalCap
			if capLimit == discountableBase {
				r = TruncatedByDiscountableFloor
			}
			reason = &r
		}

		runningTotal, err = runningTotal.Add(applied)
		if err != nil {
			return nil, shared.Internal("discount.total_overflow", err, "accumulating discount total")
		}

		results = append(results, Result{
			Candidate:        c,
			FrozenBase:       base,
			ComputedAmount:   computed,
			AppliedAmount:    applied,
			TruncationReason: reason,
			Sequence:         int16(i + 1),
		})
	}

	net, err := gross.Sub(runningTotal)
	if err != nil {
		return nil, shared.Internal("discount.net_computation_failed", err, "computing the net total")
	}
	if net.IsNegative() {
		// Unreachable while the floor holds; asserted because a negative net
		// would mean the university owes the student for enrolling.
		return nil, shared.InvariantViolation("discount.negative_net",
			"discounts of %s exceed gross fees of %s", runningTotal, gross).
			WithDetail("gross", gross.Int64()).
			WithDetail("discount_total", runningTotal.Int64())
	}

	return &Computation{
		GrossTotal:       gross,
		DiscountableBase: discountableBase,
		Applications:     results,
		TotalDiscount:    runningTotal,
		NetTotal:         net,
	}, nil
}

func totals(components []ComponentAmount) (gross, discountable money.Amount, err error) {
	for _, c := range components {
		if c.Amount.IsNegative() {
			return 0, 0, shared.Validation("discount.negative_component",
				"fee component %q has a negative amount", c.Code)
		}
		gross, err = gross.Add(c.Amount)
		if err != nil {
			return 0, 0, shared.Internal("discount.gross_overflow", err, "summing fee components")
		}
		if c.Discountable {
			discountable, err = discountable.Add(c.Amount)
			if err != nil {
				return 0, 0, shared.Internal("discount.base_overflow", err, "summing the discountable base")
			}
		}
	}
	return gross, discountable, nil
}

// baseFor narrows the discountable base to the components a definition targets.
func baseFor(c Candidate, components []ComponentAmount, fullBase money.Amount) (money.Amount, error) {
	if len(c.AppliesToComponents) == 0 {
		return fullBase, nil
	}
	targeted := make(map[string]bool, len(c.AppliesToComponents))
	for _, code := range c.AppliesToComponents {
		targeted[code] = true
	}
	var base money.Amount
	for _, comp := range components {
		if !comp.Discountable || !targeted[comp.Code] {
			continue
		}
		next, err := base.Add(comp.Amount)
		if err != nil {
			return 0, shared.Internal("discount.component_base_overflow", err,
				"summing the base for discount %s", c.DefinitionCode)
		}
		base = next
	}
	return base, nil
}

func computeRaw(c Candidate, base money.Amount) (money.Amount, error) {
	switch c.ValueType {
	case ValuePercentage:
		amount, err := money.ApplyRate(base, c.Rate)
		if err != nil {
			return 0, shared.Internal("discount.rate_computation_failed", err,
				"applying %s to %s for discount %s", c.Rate, base, c.DefinitionCode)
		}
		return amount, nil
	case ValueFixed:
		// A fixed grant larger than the base it targets delivers the base and
		// nothing more; the surplus is not carried anywhere.
		return money.Min(c.FixedAmount, base), nil
	default:
		return 0, shared.Validation("discount.unknown_value_type",
			"discount %s has unknown value type %q", c.DefinitionCode, c.ValueType)
	}
}

// screenExclusivity rejects combinations the definitions forbid, before any
// arithmetic runs.
func screenExclusivity(candidates []Candidate) ([]Candidate, error) {
	if len(candidates) <= 1 {
		return candidates, nil
	}

	for _, c := range candidates {
		if !c.Stackable && len(candidates) > 1 {
			others := make([]string, 0, len(candidates)-1)
			for _, other := range candidates {
				if other.AssignmentID != c.AssignmentID {
					others = append(others, other.DefinitionCode)
				}
			}
			return nil, shared.PreconditionFailed("discount.not_stackable",
				"discount %s cannot be combined with any other, but %v are also granted",
				c.DefinitionCode, others).
				WithDetail("definition", c.DefinitionCode).
				WithDetail("conflicts_with", others)
		}
	}

	seenGroup := make(map[shared.ID]string, len(candidates))
	for _, c := range candidates {
		if c.ExclusivityGroupID == nil {
			continue
		}
		if previous, clash := seenGroup[*c.ExclusivityGroupID]; clash {
			return nil, shared.PreconditionFailed("discount.exclusivity_conflict",
				"discounts %s and %s belong to the same exclusivity group and cannot both apply",
				previous, c.DefinitionCode).
				WithDetail("first", previous).
				WithDetail("second", c.DefinitionCode)
		}
		seenGroup[*c.ExclusivityGroupID] = c.DefinitionCode
	}

	return candidates, nil
}

// sortCandidates orders percentages before fixed amounts, then by priority,
// then by definition code so the order is total and reproducible.
func sortCandidates(candidates []Candidate) {
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if (a.ValueType == ValuePercentage) != (b.ValueType == ValuePercentage) {
			return a.ValueType == ValuePercentage
		}
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		return a.DefinitionCode < b.DefinitionCode
	})
}
