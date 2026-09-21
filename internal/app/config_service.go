package app

import (
	"cmp"
	"context"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"flowed/internal/domain/academic"
	"flowed/internal/domain/billing"
	"flowed/internal/domain/discount"
	"flowed/internal/domain/money"
	"flowed/internal/domain/shared"
	"flowed/internal/port"
)

// ConfigService administers everything the pricing engine reads: fee policies,
// installment templates, discount definitions, and the reference data all
// three are scoped against.
//
// The system's central claim is that fees are data rather than code — next
// year's prices are rows, not a deployment. That claim only holds if there is
// a way in that is not a database client. A psql session has no authority
// check, no audit entry and no validation beyond what a CHECK constraint can
// express, so configuring the system that way gives up most of the guarantees
// the rest of the design is built on, and does it silently. Every command here
// exists so that the supported route is also the easy one.
type ConfigService struct {
	deps Deps
	auditor
}

// NewConfigService wires the configuration commands.
func NewConfigService(d Deps) *ConfigService {
	return &ConfigService{deps: d, auditor: newAuditor(d.Audit, d.Clock)}
}

// configCodePattern mirrors ck_fee_component_code and ck_installment_template_code.
//
// It is restated here so that a mistyped code is refused with a message naming
// the offending component, rather than reaching the database and returning as
// a check-constraint violation that quotes a regular expression at somebody
// entering next year's tuition.
var configCodePattern = regexp.MustCompile(`^[A-Z0-9_]{2,32}$`)

// ---------------------------------------------------------------------------
// Fee policies
// ---------------------------------------------------------------------------

// FeeComponentInput is one charge inside a policy being defined.
type FeeComponentInput struct {
	Code   string
	NameAr string
	NameEn *string
	Amount money.Amount
	// IsDiscountable, IsRefundable and IsMandatory are pointers because their
	// safe default is true, matching the schema. An omitted field must not
	// quietly make a registration charge optional or a tuition charge
	// non-discountable: those are exactly the errors nobody notices until a
	// full exemption zeroes a fee that should still have been collected.
	IsDiscountable *bool
	IsRefundable   *bool
	IsMandatory    *bool
	SortOrder      int16
}

// DefineFeePolicyInput describes a new draft policy and its components.
type DefineFeePolicyInput struct {
	PolicyCode string
	VersionNo  int32

	// A nil dimension is a wildcard. The academic year never is: prices belong
	// to a year.
	AcademicYearID    shared.ID
	CollegeID         *shared.ID
	DepartmentID      *shared.ID
	Stage             *int16
	StudyTypeID       *shared.ID
	StudentCategoryID *shared.ID

	// MaxDiscountBP caps total discount as a rate of the discountable base.
	// Nil means the full rate; zero, which is a legitimate "no discounts under
	// this policy", must therefore be stated explicitly rather than arrived at
	// by leaving a field out.
	MaxDiscountBP *money.BasisPoints
	EffectiveFrom *shared.Date
	Description   *string

	Components []FeeComponentInput
}

// DefineFeePolicy records a draft policy with its components.
//
// It is a draft because nothing here is in force yet: a draft is invisible to
// resolution, so a half-entered price list cannot charge anybody while the
// person entering it goes to lunch.
func (s *ConfigService) DefineFeePolicy(ctx context.Context, actor shared.Actor, in DefineFeePolicyInput) (*billing.FeePolicy, error) {
	policyCode := strings.TrimSpace(in.PolicyCode)
	if policyCode == "" {
		return nil, shared.Validation("fee_policy.code_required",
			"a fee policy needs a code; it is what a later version supersedes by name")
	}
	if err := s.checkStage(in.Stage); err != nil {
		return nil, err
	}

	maxDiscount := money.FullRate
	if in.MaxDiscountBP != nil {
		validated, err := money.NewBasisPoints(in.MaxDiscountBP.Int32())
		if err != nil {
			return nil, shared.Validation("fee_policy.invalid_max_discount",
				"the discount ceiling must be between 0 and 100%%, got %d basis points",
				in.MaxDiscountBP.Int32()).WithCause(err)
		}
		maxDiscount = validated
	}

	var policy *billing.FeePolicy
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		year, err := s.deps.Years.GetByID(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}
		if err := s.requireConfigurableYear(year, "defining a fee policy"); err != nil {
			return err
		}
		if err := s.checkDepartmentInCollege(ctx, in.CollegeID, in.DepartmentID); err != nil {
			return err
		}

		components, err := s.buildComponents(in.Components)
		if err != nil {
			return err
		}

		versionNo := in.VersionNo
		if versionNo < 1 {
			versionNo = 1
		}

		policy = &billing.FeePolicy{
			ID:                shared.NewID(),
			PolicyCode:        policyCode,
			VersionNo:         versionNo,
			AcademicYearID:    in.AcademicYearID,
			CollegeID:         in.CollegeID,
			DepartmentID:      in.DepartmentID,
			Stage:             in.Stage,
			StudyTypeID:       in.StudyTypeID,
			StudentCategoryID: in.StudentCategoryID,
			Status:            billing.PolicyDraft,
			MaxDiscountBP:     maxDiscount,
			EffectiveFrom:     in.EffectiveFrom,
			Description:       in.Description,
			Components:        components,
			CreatedBy:         &actor.UserID,
		}
		// The store re-reads the generated column; computing it here as well
		// means a caller holding the returned struct sees the same number the
		// database will rank it by.
		policy.SpecificityScore = policy.ComputeSpecificity()

		if err := s.deps.FeePolicies.Create(ctx, policy); err != nil {
			return err
		}

		gross, err := policy.GrossTotal()
		if err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "fee_policy",
			EntityID:       &policy.ID,
			Action:         "fee_policy.defined",
			Actor:          actor,
			After:          snapshotOf(policy),
			AcademicYearID: &policy.AcademicYearID,
			Metadata: map[string]any{
				"policy_code":       policy.PolicyCode,
				"version_no":        policy.VersionNo,
				"specificity_score": policy.SpecificityScore,
				"components":        len(policy.Components),
				"gross_total":       gross.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// PublishFeePolicy puts a draft policy in force and freezes its amounts.
//
// Publishing rather than editing is what keeps a price change from reaching an
// account that already exists. An account holds copied snapshot lines, not a
// pointer to "the current price", and a published policy's components never
// move again — so raising tuition next week is a new policy, and last week's
// accounts remain explainable by the row that produced them.
func (s *ConfigService) PublishFeePolicy(ctx context.Context, actor shared.Actor, policyID shared.ID) (*billing.FeePolicy, error) {
	var policy *billing.FeePolicy
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		policy, err = s.deps.FeePolicies.GetByID(ctx, policyID)
		if err != nil {
			return err
		}
		before := snapshotOf(policy)

		if err := s.refuseDuplicateScope(ctx, policy); err != nil {
			return err
		}
		if err := policy.Publish(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.FeePolicies.Publish(ctx, policy.ID, actor.UserID, now); err != nil {
			return s.explainScopeClash(err, policy)
		}

		gross, err := policy.GrossTotal()
		if err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType:     "fee_policy",
			EntityID:       &policy.ID,
			Action:         "fee_policy.published",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(policy),
			AcademicYearID: &policy.AcademicYearID,
			Metadata: map[string]any{
				"policy_code":       policy.PolicyCode,
				"specificity_score": policy.SpecificityScore,
				"scope":             s.policyScopeKey(policy),
				"gross_total":       gross.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// RetireFeePolicy takes a published policy out of resolution, freeing its
// scope for a new version.
//
// This is what makes a configured price actually changeable without touching
// code: publishing a second policy over the same scope is refused by
// uq_fee_policy_scope while the first still holds it, so a price change is
// retire-then-publish, never an edit. Retiring never reaches into accounts
// already generated — those hold copied snapshot lines and a reference to
// this exact version, not to "whichever policy currently resolves this
// scope".
func (s *ConfigService) RetireFeePolicy(ctx context.Context, actor shared.Actor, policyID shared.ID) (*billing.FeePolicy, error) {
	var policy *billing.FeePolicy
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		policy, err = s.deps.FeePolicies.GetByID(ctx, policyID)
		if err != nil {
			return err
		}
		before := snapshotOf(policy)

		if err := policy.Retire(now); err != nil {
			return err
		}
		if err := s.deps.FeePolicies.Retire(ctx, policy.ID, now); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "fee_policy",
			EntityID:       &policy.ID,
			Action:         "fee_policy.retired",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(policy),
			AcademicYearID: &policy.AcademicYearID,
			Metadata: map[string]any{
				"policy_code": policy.PolicyCode,
				"scope":       s.policyScopeKey(policy),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// StudyTypeDebtComponentCode is the one fee component every study-type
// default-debt policy carries, so a caller can pick these policies out of a
// year's list without guessing at a naming convention.
const StudyTypeDebtComponentCode = "INITIAL_DEBT"

const studyTypeDebtPolicyPrefix = "DEFAULT_DEBT_"

// SetStudyTypeInitialDebtInput names the flat amount a newly created
// enrollment of one study type should be priced at, for one academic year.
type SetStudyTypeInitialDebtInput struct {
	AcademicYearID shared.ID
	StudyTypeID    shared.ID
	Amount         money.Amount
}

// SetStudyTypeInitialDebt is the configurable default this feature asked for:
// a study-type-only wildcard fee policy — every other scope dimension left
// null — so a new enrollment of that study type prices to this figure unless
// a more specific published policy (naming an actual college, department,
// stage or category) exists, which still wins on specificity exactly as any
// other pair of overlapping policies would.
//
// It is not a parallel balance system: it defines and publishes an ordinary
// fee_policy_version through the same engine every other price goes through,
// so the number is only ever spent through GenerateFinancialAccount's normal
// resolution, snapshot and installment path. Changing the amount later is
// retire-then-publish, in one transaction, so resolution is never briefly
// left with two published policies at this scope, nor with none.
func (s *ConfigService) SetStudyTypeInitialDebt(ctx context.Context, actor shared.Actor, in SetStudyTypeInitialDebtInput) (*billing.FeePolicy, error) {
	components, err := s.buildComponents([]FeeComponentInput{{
		Code:   StudyTypeDebtComponentCode,
		NameAr: "التزام ابتدائي",
		Amount: in.Amount,
	}})
	if err != nil {
		return nil, err
	}

	var policy *billing.FeePolicy
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		studyType, err := s.deps.Reference.GetStudyType(ctx, in.StudyTypeID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetByID(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}
		if err := s.requireConfigurableYear(year, "configuring a study type's default debt"); err != nil {
			return err
		}

		existing, err := s.deps.FeePolicies.List(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}
		// uq_fee_policy_code_version has no academic_year_id column — it is
		// unique on (policy_code, version_no) across every year — so the code
		// must carry the year itself, or a second year's first version of
		// "this study type's default" collides with the first year's.
		policyCode := studyTypeDebtPolicyPrefix + year.Code + "_" + studyType.Code
		var previous *billing.FeePolicy
		nextVersion := int32(1)
		for _, p := range existing {
			if p.PolicyCode != policyCode {
				continue
			}
			if p.VersionNo >= nextVersion {
				nextVersion = p.VersionNo + 1
			}
			if p.Status == billing.PolicyPublished {
				previous = p
			}
		}

		now := nowOr(s.deps.Clock)
		if previous != nil {
			if err := previous.Retire(now); err != nil {
				return err
			}
			if err := s.deps.FeePolicies.Retire(ctx, previous.ID, now); err != nil {
				return err
			}
			if err := s.record(ctx, port.AuditEntry{
				EntityType:     "fee_policy",
				EntityID:       &previous.ID,
				Action:         "fee_policy.retired",
				Actor:          actor,
				AcademicYearID: &year.ID,
				Metadata: map[string]any{
					"policy_code": previous.PolicyCode,
					"reason":      "superseded by a new default debt amount",
				},
			}); err != nil {
				return err
			}
		}

		policy = &billing.FeePolicy{
			ID:             shared.NewID(),
			PolicyCode:     policyCode,
			VersionNo:      nextVersion,
			AcademicYearID: year.ID,
			StudyTypeID:    &studyType.ID,
			Status:         billing.PolicyDraft,
			MaxDiscountBP:  money.FullRate,
			Description:    ptr("default debt on creation — " + studyType.NameAr),
			Components:     components,
			CreatedBy:      &actor.UserID,
		}
		policy.SpecificityScore = policy.ComputeSpecificity()

		if err := s.deps.FeePolicies.Create(ctx, policy); err != nil {
			return err
		}
		if err := policy.Publish(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.FeePolicies.Publish(ctx, policy.ID, actor.UserID, now); err != nil {
			return s.explainScopeClash(err, policy)
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "fee_policy",
			EntityID:       &policy.ID,
			Action:         "fee_policy.defined",
			Actor:          actor,
			After:          snapshotOf(policy),
			AcademicYearID: &year.ID,
			Metadata: map[string]any{
				"policy_code": policy.PolicyCode,
				"version_no":  policy.VersionNo,
				"study_type":  studyType.Code,
				"amount":      in.Amount.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return policy, nil
}

// ListFeePolicies returns every policy defined for a year, draft and published
// alike, with their components.
func (s *ConfigService) ListFeePolicies(ctx context.Context, actor shared.Actor, yearID shared.ID) ([]*billing.FeePolicy, error) {
	return s.deps.FeePolicies.List(ctx, yearID)
}

// GetFeePolicy returns one policy with its components.
func (s *ConfigService) GetFeePolicy(ctx context.Context, actor shared.Actor, policyID shared.ID) (*billing.FeePolicy, error) {
	return s.deps.FeePolicies.GetByID(ctx, policyID)
}

// FeeResolutionInput is the enrollment context a preview is run against.
type FeeResolutionInput struct {
	AcademicYearID shared.ID
	CollegeID      shared.ID
	DepartmentID   shared.ID
	Stage          int16
	StudyTypeID    shared.ID
	// StudentCategoryCode is a code rather than an identifier because that is
	// how the categories are spoken about — REGULAR, REPEAT, HOSTED. Empty
	// means REGULAR, matching what enrollment derives for a first attempt.
	StudentCategoryCode string
}

// FeeResolutionCandidate is one published policy that covers a scope.
type FeeResolutionCandidate struct {
	Policy           *billing.FeePolicy
	SpecificityScore int32
	// Dimensions names the scope columns this policy fixes rather than leaving
	// as a wildcard. It is the derivation of the score: a policy naming
	// college, department and stage scores 16+8+4, and seeing that spelled out
	// is the difference between trusting the number and checking it.
	Dimensions []string
	GrossTotal money.Amount
	Wins       bool
}

// FeeResolutionPreview reports which policy would price an enrollment, and
// which ones it beat.
type FeeResolutionPreview struct {
	Scope     port.FeeScope
	Winner    *FeeResolutionCandidate
	RunnersUp []*FeeResolutionCandidate
}

// PreviewFeeResolution answers "why is this student charged this amount",
// before anybody is charged.
//
// Resolution picks the highest specificity score among the published policies
// whose wildcards cover the enrollment. That rule is deterministic but it is
// not obvious from a table of rows, and the question it answers is asked most
// often in the worst circumstances: a student at the counter disputing a
// figure, an auditor comparing two students in the same department. Answering
// it only after an account exists means answering it only after the mistake
// has been made, so the same ranking is exposed here as a read: the winner,
// its score, and the runners-up with theirs.
//
// The ranking is computed in memory rather than delegated to the resolver
// because the resolver returns one row by design. The arithmetic is safe to
// mirror: the weights are powers of two, so the score the database generates
// and the score the domain computes cannot disagree.
func (s *ConfigService) PreviewFeeResolution(ctx context.Context, actor shared.Actor, in FeeResolutionInput) (*FeeResolutionPreview, error) {
	var preview *FeeResolutionPreview
	// One read transaction across the category lookup and the policy list, so
	// a policy published between the two queries cannot produce a ranking that
	// never existed.
	err := s.deps.Tx.Read(ctx, func(ctx context.Context) error {
		categoryCode := strings.ToUpper(strings.TrimSpace(in.StudentCategoryCode))
		if categoryCode == "" {
			categoryCode = academic.CategoryRegular
		}
		category, err := s.deps.Reference.GetStudentCategoryByCode(ctx, categoryCode)
		if err != nil {
			return err
		}
		if err := s.checkDepartmentInCollege(ctx, &in.CollegeID, &in.DepartmentID); err != nil {
			return err
		}

		scope := port.FeeScope{
			AcademicYearID:    in.AcademicYearID,
			CollegeID:         in.CollegeID,
			DepartmentID:      in.DepartmentID,
			Stage:             in.Stage,
			StudyTypeID:       in.StudyTypeID,
			StudentCategoryID: category.ID,
		}

		policies, err := s.deps.FeePolicies.List(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}

		candidates := make([]*FeeResolutionCandidate, 0, len(policies))
		for _, p := range policies {
			if p.Status != billing.PolicyPublished || !s.policyCovers(p, scope) {
				continue
			}
			gross, err := p.GrossTotal()
			if err != nil {
				return err
			}
			candidates = append(candidates, &FeeResolutionCandidate{
				Policy:           p,
				SpecificityScore: p.ComputeSpecificity(),
				Dimensions:       s.dimensionsOf(p),
				GrossTotal:       gross,
			})
		}

		// Highest score first. Two published policies cannot tie on score
		// while uq_fee_policy_scope holds, but the code is not entitled to
		// assume the constraint was never dropped, so the order is made total
		// by the policy code.
		slices.SortFunc(candidates, func(a, b *FeeResolutionCandidate) int {
			if byScore := cmp.Compare(b.SpecificityScore, a.SpecificityScore); byScore != 0 {
				return byScore
			}
			return cmp.Compare(a.Policy.PolicyCode, b.Policy.PolicyCode)
		})

		preview = &FeeResolutionPreview{Scope: scope}
		if len(candidates) > 0 {
			candidates[0].Wins = true
			preview.Winner = candidates[0]
			preview.RunnersUp = candidates[1:]
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return preview, nil
}

// policyCovers reports whether a policy's scope reaches an enrollment. A nil
// dimension is a wildcard, exactly as the resolver's SQL reads it.
func (s *ConfigService) policyCovers(p *billing.FeePolicy, scope port.FeeScope) bool {
	if p.AcademicYearID != scope.AcademicYearID {
		return false
	}
	if p.CollegeID != nil && *p.CollegeID != scope.CollegeID {
		return false
	}
	if p.DepartmentID != nil && *p.DepartmentID != scope.DepartmentID {
		return false
	}
	if p.Stage != nil && *p.Stage != scope.Stage {
		return false
	}
	if p.StudyTypeID != nil && *p.StudyTypeID != scope.StudyTypeID {
		return false
	}
	if p.StudentCategoryID != nil && *p.StudentCategoryID != scope.StudentCategoryID {
		return false
	}
	return true
}

// dimensionsOf names the scope columns a policy fixes.
func (s *ConfigService) dimensionsOf(p *billing.FeePolicy) []string {
	dimensions := make([]string, 0, 5)
	if p.CollegeID != nil {
		dimensions = append(dimensions, "college")
	}
	if p.DepartmentID != nil {
		dimensions = append(dimensions, "department")
	}
	if p.Stage != nil {
		dimensions = append(dimensions, "stage")
	}
	if p.StudyTypeID != nil {
		dimensions = append(dimensions, "study_type")
	}
	if p.StudentCategoryID != nil {
		dimensions = append(dimensions, "student_category")
	}
	return dimensions
}

// refuseDuplicateScope checks, before publishing, the rule the database also
// enforces with uq_fee_policy_scope.
//
// That index — and its NULLS NOT DISTINCT clause — permits exactly one
// published policy per scope, and the single-winner property is what makes
// resolution deterministic: two published rows both meaning "2025-2026,
// engineering, any department" would match the same student at the same
// specificity, and nothing in the ranking could choose between them. The check
// here is not redundant with the index. It runs before the domain freezes the
// policy, and it can name the policy already holding the scope, which a
// unique-violation cannot.
func (s *ConfigService) refuseDuplicateScope(ctx context.Context, policy *billing.FeePolicy) error {
	existing, err := s.deps.FeePolicies.List(ctx, policy.AcademicYearID)
	if err != nil {
		return err
	}
	key := s.policyScopeKey(policy)
	for _, other := range existing {
		if other.ID == policy.ID || other.Status != billing.PolicyPublished {
			continue
		}
		if s.policyScopeKey(other) != key {
			continue
		}
		return s.duplicateScopeError(policy, other.PolicyCode)
	}
	return nil
}

// explainScopeClash turns the driver's unique violation into an error that
// says what the constraint is for, for the case where a concurrent publication
// wins the race after refuseDuplicateScope has already looked.
func (s *ConfigService) explainScopeClash(err error, policy *billing.FeePolicy) error {
	domainErr, ok := shared.AsDomain(err)
	if !ok || domainErr.Code != "fee_policy.duplicate_scope" {
		return err
	}
	return s.duplicateScopeError(policy, "").WithCause(err)
}

func (s *ConfigService) duplicateScopeError(policy *billing.FeePolicy, holder string) *shared.Error {
	message := "a published fee policy already covers exactly this scope; resolution must have a single " +
		"winner, because two published policies at the same specificity would both match a student and " +
		"nothing could choose between them"
	if holder != "" {
		message = "fee policy " + holder + " is already published over exactly this scope; resolution must " +
			"have a single winner, because two published policies at the same specificity would both match " +
			"a student and nothing could choose between them"
	}
	err := shared.Conflict("fee_policy.duplicate_scope", "%s", message).
		WithDetail("policy_code", policy.PolicyCode).
		WithDetail("scope", s.policyScopeKey(policy)).
		WithDetail("specificity_score", policy.ComputeSpecificity()).
		WithDetail("remedy", "retire the policy holding this scope, or narrow this one by naming a further dimension")
	if holder != "" {
		err = err.WithDetail("held_by_policy_code", holder)
	}
	return err
}

// policyScopeKey renders a policy's scope as a comparable string, with * for
// each wildcard so that the value reads the way the schema means it.
func (s *ConfigService) policyScopeKey(p *billing.FeePolicy) string {
	return strings.Join([]string{
		p.AcademicYearID.String(),
		s.idKey(p.CollegeID),
		s.idKey(p.DepartmentID),
		s.stageKey(p.Stage),
		s.idKey(p.StudyTypeID),
		s.idKey(p.StudentCategoryID),
	}, "|")
}

func (s *ConfigService) idKey(id *shared.ID) string {
	if id == nil {
		return "*"
	}
	return id.String()
}

func (s *ConfigService) stageKey(stage *int16) string {
	if stage == nil {
		return "*"
	}
	return strconv.Itoa(int(*stage))
}

// buildComponents validates and normalises the components of a draft policy.
func (s *ConfigService) buildComponents(inputs []FeeComponentInput) ([]*billing.FeeComponent, error) {
	if len(inputs) == 0 {
		return nil, shared.Validation("fee_policy.no_components",
			"a fee policy must define at least one component; one with none prices an enrollment at nothing")
	}

	components := make([]*billing.FeeComponent, 0, len(inputs))
	seen := make(map[string]bool, len(inputs))
	for i, in := range inputs {
		code := strings.ToUpper(strings.TrimSpace(in.Code))
		if !configCodePattern.MatchString(code) {
			return nil, shared.Validation("fee_policy.invalid_component_code",
				"a component code must be 2 to 32 upper-case letters, digits or underscores, got %q", in.Code).
				WithDetail("component_code", in.Code)
		}
		if seen[code] {
			// The snapshot copied onto an account is keyed by component code,
			// so a policy carrying the code twice would produce an account
			// whose lines cannot be told apart or reconciled against it.
			return nil, shared.Validation("fee_policy.duplicate_component",
				"component %q appears more than once", code).
				WithDetail("component_code", code)
		}
		seen[code] = true

		if strings.TrimSpace(in.NameAr) == "" {
			return nil, shared.Validation("fee_policy.component_name_required",
				"component %q needs an Arabic name; it is printed on the student's statement", code).
				WithDetail("component_code", code)
		}
		if in.Amount.IsNegative() {
			return nil, shared.Validation("fee_policy.negative_component",
				"component %q has a negative amount (%s); a charge that pays the student is an adjustment, not a fee",
				code, in.Amount).
				WithDetail("component_code", code).
				WithDetail("amount", in.Amount.Int64())
		}

		// An unset sort order falls back to the order the components were
		// entered in, which is the order the person entering them meant.
		sortOrder := in.SortOrder
		if sortOrder == 0 {
			sortOrder = int16(i)
		}

		components = append(components, &billing.FeeComponent{
			ID:             shared.NewID(),
			Code:           code,
			NameAr:         strings.TrimSpace(in.NameAr),
			NameEn:         in.NameEn,
			Amount:         in.Amount,
			IsDiscountable: s.boolOr(in.IsDiscountable, true),
			IsRefundable:   s.boolOr(in.IsRefundable, true),
			IsMandatory:    s.boolOr(in.IsMandatory, true),
			SortOrder:      sortOrder,
		})
	}
	return components, nil
}

// ---------------------------------------------------------------------------
// Installment templates
// ---------------------------------------------------------------------------

// TemplateLineInput is one weighted share of a plan shape.
type TemplateLineInput struct {
	// LineNo may be left at zero, in which case the lines are numbered in the
	// order they were given.
	LineNo int16
	// Exactly one of ShareBP or Amount is used: Amount when every line on the
	// template supplies one — "400,000 / 400,000 / 350,000 / 350,000" — in
	// which case ShareBP is derived rather than taken from the caller, so a
	// plan generated from it reproduces the typed figures exactly instead of
	// re-deriving them through a percentage. ShareBP alone is the ordinary
	// percentage template.
	ShareBP money.BasisPoints
	Amount  *money.Amount
	// DueOffsetDays counts from the academic year's start, so one template
	// serves every year.
	DueOffsetDays int
	Label         *string
}

// DefineInstallmentTemplateInput describes a new draft plan shape.
type DefineInstallmentTemplateInput struct {
	Code   string
	NameAr string
	NameEn *string

	// Every dimension is optional here, the academic year included: a template
	// with no year is global and serves each year until something narrower
	// overrides it.
	AcademicYearID *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	Stage          *int16
	StudyTypeID    *shared.ID

	// MaxInstallments defaults to the number of lines supplied.
	MaxInstallments int16
	Lines           []TemplateLineInput
}

// DefineInstallmentTemplate records a draft plan shape with its lines.
//
// The shares are deliberately not required to total 100% here. A draft is
// allowed to be half-entered — that is what a draft is for — and the check
// that matters runs at publication, where the administrator is told the total
// they actually typed.
func (s *ConfigService) DefineInstallmentTemplate(ctx context.Context, actor shared.Actor, in DefineInstallmentTemplateInput) (*billing.InstallmentTemplate, error) {
	code := strings.ToUpper(strings.TrimSpace(in.Code))
	if !configCodePattern.MatchString(code) {
		return nil, shared.Validation("installment_template.invalid_code",
			"a template code must be 2 to 32 upper-case letters, digits or underscores, got %q", in.Code)
	}
	if strings.TrimSpace(in.NameAr) == "" {
		return nil, shared.Validation("installment_template.name_required",
			"the Arabic name is required; it is what a cashier picks the plan by")
	}
	if err := s.checkStage(in.Stage); err != nil {
		return nil, err
	}

	lines, err := s.buildTemplateLines(in.Lines)
	if err != nil {
		return nil, err
	}

	maxInstallments := in.MaxInstallments
	if maxInstallments < 1 {
		maxInstallments = int16(len(lines))
	}
	if maxInstallments > 24 {
		return nil, shared.Validation("installment_template.invalid_maximum",
			"a plan runs to at most 24 installments, got %d", maxInstallments)
	}
	if int16(len(lines)) > maxInstallments {
		return nil, shared.Validation("installment_template.too_many_lines",
			"the template defines %d installments but its maximum is %d", len(lines), maxInstallments)
	}

	var template *billing.InstallmentTemplate
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		if in.AcademicYearID != nil {
			year, err := s.deps.Years.GetByID(ctx, *in.AcademicYearID)
			if err != nil {
				return err
			}
			if err := s.requireConfigurableYear(year, "defining an installment template"); err != nil {
				return err
			}
		}
		if err := s.checkDepartmentInCollege(ctx, in.CollegeID, in.DepartmentID); err != nil {
			return err
		}

		template = &billing.InstallmentTemplate{
			ID:              shared.NewID(),
			Code:            code,
			NameAr:          strings.TrimSpace(in.NameAr),
			NameEn:          in.NameEn,
			AcademicYearID:  in.AcademicYearID,
			CollegeID:       in.CollegeID,
			DepartmentID:    in.DepartmentID,
			Stage:           in.Stage,
			StudyTypeID:     in.StudyTypeID,
			MaxInstallments: maxInstallments,
			Status:          billing.PolicyDraft,
			Lines:           lines,
		}
		if err := s.deps.Templates.Create(ctx, template); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "installment_template",
			EntityID:       &template.ID,
			Action:         "installment_template.defined",
			Actor:          actor,
			After:          snapshotOf(template),
			AcademicYearID: template.AcademicYearID,
			Metadata: map[string]any{
				"code":     template.Code,
				"lines":    len(template.Lines),
				"total_bp": int(s.totalShare(template.Lines)),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return template, nil
}

// PublishInstallmentTemplate validates the shares and puts the template in
// force.
//
// The domain refuses to publish unless the lines total exactly 10,000 basis
// points, and the check lives at publication rather than at use for a reason
// worth stating: a template whose shares come to 99% is a data-entry mistake
// that would otherwise surface once per student, at the counter, on the first
// morning of registration, with a queue in the corridor. Caught here, it costs
// one administrator one correction — provided the error tells them the total
// they actually entered, which is why the message names it.
func (s *ConfigService) PublishInstallmentTemplate(ctx context.Context, actor shared.Actor, templateID shared.ID) (*billing.InstallmentTemplate, error) {
	var template *billing.InstallmentTemplate
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		template, err = s.deps.Templates.GetByID(ctx, templateID)
		if err != nil {
			return err
		}
		before := snapshotOf(template)

		if err := s.refuseDuplicateTemplateScope(ctx, template); err != nil {
			return err
		}
		if err := template.Publish(actor.UserID, now); err != nil {
			return s.explainShareTotal(err, template)
		}
		if err := s.deps.Templates.Publish(ctx, template.ID, actor.UserID, now); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "installment_template",
			EntityID:       &template.ID,
			Action:         "installment_template.published",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(template),
			AcademicYearID: template.AcademicYearID,
			Metadata: map[string]any{
				"code":     template.Code,
				"lines":    len(template.Lines),
				"total_bp": int(s.totalShare(template.Lines)),
				"scope":    s.templateScopeKey(template),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return template, nil
}

// RetireInstallmentTemplate takes a published template out of resolution,
// freeing its scope for a new version — the installment-template counterpart
// of RetireFeePolicy, and needed for the same reason: a published template's
// scope is claimed until something retires it (uq_installment_template_scope),
// so changing a plan is retire-then-publish, never an edit.
func (s *ConfigService) RetireInstallmentTemplate(ctx context.Context, actor shared.Actor, templateID shared.ID) (*billing.InstallmentTemplate, error) {
	var template *billing.InstallmentTemplate
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		template, err = s.deps.Templates.GetByID(ctx, templateID)
		if err != nil {
			return err
		}
		before := snapshotOf(template)

		if err := template.Retire(now); err != nil {
			return err
		}
		if err := s.deps.Templates.Retire(ctx, template.ID, now); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "installment_template",
			EntityID:       &template.ID,
			Action:         "installment_template.retired",
			Actor:          actor,
			Before:         before,
			After:          snapshotOf(template),
			AcademicYearID: template.AcademicYearID,
			Metadata: map[string]any{
				"code":  template.Code,
				"scope": s.templateScopeKey(template),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return template, nil
}

// ListInstallmentTemplates returns the templates applicable to a year, or
// every template when the year is nil.
func (s *ConfigService) ListInstallmentTemplates(ctx context.Context, actor shared.Actor, yearID *shared.ID) ([]*billing.InstallmentTemplate, error) {
	return s.deps.Templates.List(ctx, yearID)
}

// GetInstallmentTemplate returns one template with its lines.
func (s *ConfigService) GetInstallmentTemplate(ctx context.Context, actor shared.Actor, templateID shared.ID) (*billing.InstallmentTemplate, error) {
	return s.deps.Templates.GetByID(ctx, templateID)
}

// StudyTypeInstallmentLineInput is one literal installment on a study type's
// default plan.
type StudyTypeInstallmentLineInput struct {
	Amount        money.Amount
	DueOffsetDays int
	Label         *string
}

// SetStudyTypeInstallmentPlanInput names the literal installments a newly
// created enrollment of one study type should be split into, for one
// academic year.
type SetStudyTypeInstallmentPlanInput struct {
	AcademicYearID shared.ID
	StudyTypeID    shared.ID
	Lines          []StudyTypeInstallmentLineInput
}

// installmentPlanCodePrefix mirrors studyTypeDebtPolicyPrefix: a template
// code namespaced by this feature so SetStudyTypeInstallmentPlan can find its
// own previous versions without touching a template a finance manager
// defined by hand over the same scope. Unlike a fee policy, a template has no
// version_no column of its own — uq_installment_template_code is unique on
// the bare code, retired rows included — so every call gets a code carrying
// a fresh numeric suffix rather than reusing one.
// Short because installment_template.code has only 32 characters to spend in
// total, split between this, a compact fragment of two ids and a version
// suffix — see compactID.
const installmentPlanCodePrefix = "PLAN_"

// compactID renders a short, fixed-length, deterministic fragment of a UUID
// for use inside a code that has to stay within a tight length limit
// (installment_template.code, capped at 32 characters by
// ck_installment_template_code) while still being reproducible from the id
// alone, so a later call can find what an earlier one created.
func compactID(id shared.ID) string {
	return strings.ToUpper(strings.ReplaceAll(id.String(), "-", "")[:8])
}

// SetStudyTypeInstallmentPlan is the configuration surface for a study type's
// default installment plan: a study-type-only wildcard installment template —
// every other scope dimension left null — authored in the literal amounts an
// administrator actually means ("400,000 / 400,000 / 350,000 / 350,000"),
// not percentages.
//
// It reuses the same installment-template engine every other plan goes
// through: retiring the previous version (if any) then defining and
// publishing a new one, all in one transaction, exactly the pattern
// SetStudyTypeInitialDebt already uses for the fee side.
func (s *ConfigService) SetStudyTypeInstallmentPlan(ctx context.Context, actor shared.Actor, in SetStudyTypeInstallmentPlanInput) (*billing.InstallmentTemplate, error) {
	lineInputs := make([]TemplateLineInput, len(in.Lines))
	for i, line := range in.Lines {
		amount := line.Amount
		lineInputs[i] = TemplateLineInput{
			Amount:        &amount,
			DueOffsetDays: line.DueOffsetDays,
			Label:         line.Label,
		}
	}
	lines, err := s.buildTemplateLines(lineInputs)
	if err != nil {
		return nil, err
	}

	var template *billing.InstallmentTemplate
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		studyType, err := s.deps.Reference.GetStudyType(ctx, in.StudyTypeID)
		if err != nil {
			return err
		}
		year, err := s.deps.Years.GetByID(ctx, in.AcademicYearID)
		if err != nil {
			return err
		}
		if err := s.requireConfigurableYear(year, "configuring a study type's default installment plan"); err != nil {
			return err
		}

		existing, err := s.deps.Templates.List(ctx, &in.AcademicYearID)
		if err != nil {
			return err
		}
		// installment_template has no version_no column the way
		// fee_policy_version does — uq_installment_template_code is unique on
		// the bare code, retired rows included — so a second call for this
		// exact scope cannot reuse the first call's code at all; it needs a
		// fresh one. The base already carries the year, since
		// uq_installment_template_code has no academic_year_id column either.
		// ck_installment_template_code caps a code at 32 upper-case letters,
		// digits and underscores — no hyphens, and too tight to spell out a
		// year and a study-type code (which is itself allowed up to 32
		// characters) side by side without risking overflow. A short,
		// deterministic fragment of each id keeps the composite code fixed
		// in length regardless of how either is named; the year and study
		// type themselves are recorded properly in the template's own
		// columns; the code only has to be able to find its own prior
		// versions again.
		base := installmentPlanCodePrefix + compactID(year.ID) + "_" + compactID(studyType.ID)
		nextVersion := 1
		var previous *billing.InstallmentTemplate
		for _, t := range existing {
			if !strings.HasPrefix(t.Code, base) {
				continue
			}
			nextVersion++
			if t.Status == billing.PolicyPublished {
				previous = t
			}
		}
		code := base + "_V" + strconv.Itoa(nextVersion)

		now := nowOr(s.deps.Clock)
		if previous != nil {
			if err := previous.Retire(now); err != nil {
				return err
			}
			if err := s.deps.Templates.Retire(ctx, previous.ID, now); err != nil {
				return err
			}
			if err := s.record(ctx, port.AuditEntry{
				EntityType:     "installment_template",
				EntityID:       &previous.ID,
				Action:         "installment_template.retired",
				Actor:          actor,
				AcademicYearID: previous.AcademicYearID,
				Metadata: map[string]any{
					"code":   previous.Code,
					"reason": "superseded by a new default installment plan",
				},
			}); err != nil {
				return err
			}
		}

		template = &billing.InstallmentTemplate{
			ID:              shared.NewID(),
			Code:            code,
			NameAr:          "قسط افتراضي — " + studyType.NameAr,
			AcademicYearID:  &year.ID,
			StudyTypeID:     &studyType.ID,
			MaxInstallments: int16(len(lines)),
			Status:          billing.PolicyDraft,
			Lines:           lines,
		}
		if err := s.deps.Templates.Create(ctx, template); err != nil {
			return err
		}
		if err := s.refuseDuplicateTemplateScope(ctx, template); err != nil {
			return err
		}
		if err := template.Publish(actor.UserID, now); err != nil {
			return s.explainShareTotal(err, template)
		}
		if err := s.deps.Templates.Publish(ctx, template.ID, actor.UserID, now); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType:     "installment_template",
			EntityID:       &template.ID,
			Action:         "installment_template.defined",
			Actor:          actor,
			After:          snapshotOf(template),
			AcademicYearID: template.AcademicYearID,
			Metadata: map[string]any{
				"code":       template.Code,
				"study_type": studyType.Code,
				"line_count": len(lines),
				"total_bp":   int(s.totalShare(lines)),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return template, nil
}

// explainShareTotal adds the template's identity and the size of the gap to
// the domain's share failure.
//
// The domain message already names the total, which is the number that makes
// the mistake findable: shares totalling 9,900 mean one line is a percentage
// point light, and "they must total 100%" alone leaves an administrator adding
// four numbers up by hand.
func (s *ConfigService) explainShareTotal(err error, template *billing.InstallmentTemplate) error {
	domainErr, ok := shared.AsDomain(err)
	if !ok || domainErr.Code != "installment_template.shares_do_not_total" {
		return err
	}

	total := s.totalShare(template.Lines)
	gapKey, gap := "short_by_bp", money.FullRate-total
	if gap < 0 {
		gapKey, gap = "over_by_bp", -gap
	}
	return domainErr.
		WithDetail("template_code", template.Code).
		WithDetail("total_bp", int(total)).
		WithDetail(gapKey, int(gap))
}

// refuseDuplicateTemplateScope enforces, before publication,
// uq_installment_template_scope: one published template per scope, so that
// plan resolution has a single winner for the same reason fee resolution does.
func (s *ConfigService) refuseDuplicateTemplateScope(ctx context.Context, template *billing.InstallmentTemplate) error {
	existing, err := s.deps.Templates.List(ctx, template.AcademicYearID)
	if err != nil {
		return err
	}
	key := s.templateScopeKey(template)
	for _, other := range existing {
		if other.ID == template.ID || other.Status != billing.PolicyPublished {
			continue
		}
		if s.templateScopeKey(other) != key {
			continue
		}
		return shared.Conflict("installment_template.duplicate_scope",
			"installment template %s is already published over exactly this scope; plan resolution must have "+
				"a single winner", other.Code).
			WithDetail("code", template.Code).
			WithDetail("held_by_code", other.Code).
			WithDetail("scope", key).
			WithDetail("remedy", "retire the template holding this scope, or narrow this one by naming a further dimension")
	}
	return nil
}

func (s *ConfigService) templateScopeKey(t *billing.InstallmentTemplate) string {
	return strings.Join([]string{
		s.idKey(t.AcademicYearID),
		s.idKey(t.CollegeID),
		s.idKey(t.DepartmentID),
		s.stageKey(t.Stage),
		s.idKey(t.StudyTypeID),
	}, "|")
}

func (s *ConfigService) totalShare(lines []billing.TemplateLine) money.BasisPoints {
	var total money.BasisPoints
	for _, line := range lines {
		total += line.ShareBP
	}
	return total
}

// buildTemplateLines validates and numbers the lines of a draft template.
//
// A template is authored either in percentages (ShareBP on every line) or in
// literal amounts (Amount on every line) — never a mix, because a line with
// neither has no defined share and a line with both is ambiguous about which
// one is authoritative. In the literal case ShareBP is derived here, not
// taken from the caller: that is what lets a mid-year re-split, which only
// ever knows a share of a remainder, keep working from a template nobody
// entered a percentage into.
func (s *ConfigService) buildTemplateLines(inputs []TemplateLineInput) ([]billing.TemplateLine, error) {
	if len(inputs) == 0 {
		return nil, shared.Validation("installment_template.no_lines",
			"a template must define at least one installment; the lines are the template")
	}

	amountCount := 0
	for _, in := range inputs {
		if in.Amount != nil {
			amountCount++
		}
	}
	literal := amountCount > 0
	if literal && amountCount != len(inputs) {
		return nil, shared.Validation("installment_template.mixed_amount_and_share",
			"%d of %d lines carry a literal amount; a template is authored either entirely in amounts "+
				"or entirely in percentages, never a mix", amountCount, len(inputs))
	}

	var derivedShares []money.BasisPoints
	if literal {
		var err error
		derivedShares, err = sharesFromAmounts(inputs)
		if err != nil {
			return nil, err
		}
	}

	lines := make([]billing.TemplateLine, 0, len(inputs))
	seen := make(map[int16]bool, len(inputs))
	for i, in := range inputs {
		lineNo := in.LineNo
		if lineNo == 0 {
			lineNo = int16(i + 1)
		}
		if lineNo < 1 {
			return nil, shared.Validation("installment_template.invalid_line_number",
				"installment numbers start at 1, got %d", lineNo)
		}
		if seen[lineNo] {
			return nil, shared.Validation("installment_template.duplicate_line",
				"line number %d appears more than once", lineNo).
				WithDetail("line_no", int(lineNo))
		}
		seen[lineNo] = true

		shareBP := in.ShareBP
		if literal {
			shareBP = derivedShares[i]
		} else if in.ShareBP <= 0 || in.ShareBP > money.FullRate {
			return nil, shared.Validation("installment_template.invalid_share",
				"line %d has a share of %d basis points; a share is between 1 and %d",
				lineNo, in.ShareBP, money.FullRate).
				WithDetail("line_no", int(lineNo)).
				WithDetail("share_bp", int(in.ShareBP))
		}
		if in.DueOffsetDays < 0 {
			return nil, shared.Validation("installment_template.negative_offset",
				"line %d falls due %d days before the year starts", lineNo, -in.DueOffsetDays).
				WithDetail("line_no", int(lineNo))
		}

		lines = append(lines, billing.TemplateLine{
			LineNo:        lineNo,
			ShareBP:       shareBP,
			Amount:        in.Amount,
			DueOffsetDays: in.DueOffsetDays,
			Label:         in.Label,
		})
	}
	return lines, nil
}

// sharesFromAmounts derives a basis-point share for every line of a
// literal-amount template, summing to exactly money.FullRate.
//
// Every line but the last is rounded independently; the last absorbs
// whatever the rounding left over, the same convention GeneratePlan itself
// uses for the reverse operation (splitting a net back into amounts) — so a
// template round-trips through this exactly for the one net it was written
// for. This total is not required to match any other configured price: it is
// only what makes the shares valid to store, and GeneratePlan's own
// VerifyPlanSum is what refuses a mismatch against a real account's net.
func sharesFromAmounts(inputs []TemplateLineInput) ([]money.BasisPoints, error) {
	var total money.Amount
	for i, in := range inputs {
		if in.Amount == nil || in.Amount.IsNegative() || in.Amount.IsZero() {
			return nil, shared.Validation("installment_template.invalid_amount",
				"line %d has no positive amount; a literal-amount template needs one on every line", i+1).
				WithDetail("line_no", i+1)
		}
		next, err := total.Add(*in.Amount)
		if err != nil {
			return nil, shared.Internal("installment_template.amount_overflow", err, "summing template amounts")
		}
		total = next
	}

	shares := make([]money.BasisPoints, len(inputs))
	var allocated money.BasisPoints
	for i := 0; i < len(inputs)-1; i++ {
		bp, err := deriveShareBP(*inputs[i].Amount, total)
		if err != nil {
			return nil, shared.Internal("installment_template.share_arithmetic", err,
				"deriving a share for line %d", i+1)
		}
		shares[i] = bp
		allocated += bp
	}
	last := money.FullRate - allocated
	if last <= 0 || last > money.FullRate {
		return nil, shared.Validation("installment_template.invalid_amount",
			"the last installment's amount is too small relative to the others to derive a valid share").
			WithDetail("line_no", len(inputs))
	}
	shares[len(inputs)-1] = last
	return shares, nil
}

// deriveShareBP computes amount's share of total in basis points, rounded
// half-up — the same convention money.ApplyRate uses to go the other way.
func deriveShareBP(amount, total money.Amount) (money.BasisPoints, error) {
	const scale = int64(money.FullRate)
	a, t := amount.Int64(), total.Int64()
	if a > (int64(1)<<62)/scale {
		return 0, shared.Internal("installment_template.share_overflow", nil,
			"amount %d is too large for exact share arithmetic", a)
	}
	product := a * scale
	quotient := product / t
	remainder := product % t
	if remainder*2 >= t {
		quotient++
	}
	return money.BasisPoints(quotient), nil
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

// DefineDiscountInput describes a new entry in the discount catalogue.
type DefineDiscountInput struct {
	Code     string
	NameAr   string
	NameEn   *string
	Category discount.Category

	ExclusivityGroupID *shared.ID
	IsFullExemption    bool
	// AnnualReconfirmation is a pointer because its safe default is true.
	// Silently letting a hardship discount run for six years without anybody
	// re-checking the hardship is both a budget leak and an audit finding.
	AnnualReconfirmation *bool
}

// DefineDiscount creates a discount definition: the header only.
//
// A definition deliberately carries no value. Every number lives on a version,
// so raising a rate is an insert rather than an update, and no computed
// history can be reached by editing configuration.
func (s *ConfigService) DefineDiscount(ctx context.Context, actor shared.Actor, in DefineDiscountInput) (*discount.Definition, error) {
	category := in.Category
	if category == "" {
		category = discount.CategoryOther
	}
	switch category {
	case discount.CategorySocial, discount.CategoryStaff, discount.CategoryMerit,
		discount.CategoryExemption, discount.CategorySibling, discount.CategoryMartyr,
		discount.CategoryOther:
	default:
		return nil, shared.Validation("discount.invalid_category",
			"unknown discount category %q", in.Category)
	}

	var definition *discount.Definition
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		definition, err = discount.NewDefinition(in.Code, in.NameAr, category)
		if err != nil {
			return err
		}
		definition.NameEn = in.NameEn
		definition.ExclusivityGroupID = in.ExclusivityGroupID
		definition.IsFullExemption = in.IsFullExemption
		definition.AnnualReconfirmation = s.boolOr(in.AnnualReconfirmation, true)
		definition.CreatedBy = &actor.UserID

		if err := s.deps.Discounts.CreateDefinition(ctx, definition); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_definition",
			EntityID:   &definition.ID,
			Action:     "discount.definition_created",
			Actor:      actor,
			After:      snapshotOf(definition),
			Metadata: map[string]any{
				"code":                  definition.Code,
				"category":              string(definition.Category),
				"full_exemption":        definition.IsFullExemption,
				"annual_reconfirmation": definition.AnnualReconfirmation,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return definition, nil
}

// AddDiscountVersionInput describes a new draft configuration of a discount.
type AddDiscountVersionInput struct {
	DefinitionID shared.ID
	// VersionNo may be left at zero, in which case it follows the version
	// currently in force.
	VersionNo int32

	ValueType   discount.ValueType
	Rate        money.BasisPoints
	FixedAmount money.Amount

	// AppliesToComponents narrows the discount to named fee components. Empty
	// means every discountable component, which is what lets an exemption
	// cover tuition while the identity-card charge stays payable.
	AppliesToComponents []string
	PerApplicationCap   *money.Amount
	Stackable           *bool
	Priority            *int16
	RequiresApproval    *bool
	ApprovalRole        *shared.Role
	RequiredDocuments   []string

	ValidFromYearID *shared.ID
	ValidToYearID   *shared.ID
	Notes           *string
}

// AddDiscountVersion records a draft version of a discount, percentage or
// fixed.
//
// Drafts are inert: no grant can point at one, so a rate can be prepared,
// reviewed and corrected without any account seeing it.
func (s *ConfigService) AddDiscountVersion(ctx context.Context, actor shared.Actor, in AddDiscountVersionInput) (*discount.DefinitionVersion, error) {
	if in.ApprovalRole != nil && !in.ApprovalRole.Valid() {
		return nil, shared.Validation("discount.invalid_approval_role",
			"%q is not a role this system recognises", *in.ApprovalRole)
	}

	components, err := s.normaliseComponentCodes(in.AppliesToComponents)
	if err != nil {
		return nil, err
	}

	var version *discount.DefinitionVersion
	err = s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		definition, err := s.deps.Discounts.GetDefinition(ctx, in.DefinitionID)
		if err != nil {
			return err
		}

		versionNo, err := s.nextVersionNo(ctx, definition.ID, in.VersionNo)
		if err != nil {
			return err
		}

		switch in.ValueType {
		case discount.ValuePercentage:
			version, err = discount.NewPercentageVersion(definition.ID, versionNo, in.Rate)
		case discount.ValueFixed:
			version, err = discount.NewFixedVersion(definition.ID, versionNo, in.FixedAmount)
		default:
			return shared.Validation("discount.invalid_value_type",
				"a discount version is percentage or fixed, got %q", in.ValueType)
		}
		if err != nil {
			return err
		}

		version.AppliesToComponents = components
		version.PerApplicationCap = in.PerApplicationCap
		version.Stackable = s.boolOr(in.Stackable, true)
		version.RequiresApproval = s.boolOr(in.RequiresApproval, true)
		version.ApprovalRole = in.ApprovalRole
		version.RequiredDocuments = in.RequiredDocuments
		version.ValidFromYearID = in.ValidFromYearID
		version.ValidToYearID = in.ValidToYearID
		version.Notes = in.Notes
		if in.Priority != nil {
			version.Priority = *in.Priority
		}
		// Recorded so the four-eyes check at publication has somebody to
		// compare the publisher against.
		version.CreatedBy = &actor.UserID

		if err := s.deps.Discounts.CreateVersion(ctx, version); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_definition_version",
			EntityID:   &version.ID,
			Action:     "discount.version_created",
			Actor:      actor,
			After:      snapshotOf(version),
			Metadata: map[string]any{
				"definition_code": definition.Code,
				"version_no":      version.VersionNo,
				"value_type":      string(version.ValueType),
				"rate_bp":         version.Rate.Int32(),
				"fixed_amount":    version.FixedAmount.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return version, nil
}

// PublishDiscountVersion freezes a draft version and puts it in force.
//
// This is the load-bearing act for historical integrity. Every application of
// a discount to an account stores the identifier of the version it used, and a
// published version is immutable by database trigger: nothing but the
// lifecycle columns may change afterwards. So the numbers behind a discount
// computed in 2025 are still there, unaltered, when somebody audits that
// account in 2030 — not because the application saved a copy, but because the
// row it points at physically cannot have moved. Publishing is the moment that
// promise is made, and it is why a rate change is a new version rather than an
// edit to this one.
//
// It also takes two people. The actor publishing must not be the one who
// created the draft, mirroring the four-eyes rule that governs every other
// money decision in the system: a discount is revenue the university chooses
// not to collect, and one person setting a rate and putting it in force alone
// is the shape of every quiet leak.
func (s *ConfigService) PublishDiscountVersion(ctx context.Context, actor shared.Actor, versionID shared.ID) (*discount.DefinitionVersion, error) {
	var version *discount.DefinitionVersion
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		now := nowOr(s.deps.Clock)

		var err error
		version, err = s.deps.Discounts.GetVersion(ctx, versionID)
		if err != nil {
			return err
		}
		before := snapshotOf(version)

		// A version with no recorded author cannot be checked — a seeded or
		// migrated row, not a self-publication — and refusing it would leave
		// the catalogue unpublishable. The comparison is made whenever there
		// is somebody to compare against.
		if version.CreatedBy != nil && *version.CreatedBy == actor.UserID {
			return shared.Forbidden("discount.self_published_version",
				"a discount version cannot be published by the person who drafted it; ask a second "+
					"finance manager to review the value and publish it").
				WithDetail("version_id", version.ID.String()).
				WithDetail("drafted_by", version.CreatedBy.String())
		}

		if err := version.Publish(actor.UserID, now); err != nil {
			return err
		}
		if err := s.deps.Discounts.PublishVersion(ctx, version.ID, actor.UserID, now); err != nil {
			return err
		}

		return s.record(ctx, port.AuditEntry{
			EntityType: "discount_definition_version",
			EntityID:   &version.ID,
			Action:     "discount.version_published",
			Actor:      actor,
			Before:     before,
			After:      snapshotOf(version),
			Metadata: map[string]any{
				"definition_id": version.DefinitionID.String(),
				"version_no":    version.VersionNo,
				"value_type":    string(version.ValueType),
				"rate_bp":       version.Rate.Int32(),
				"fixed_amount":  version.FixedAmount.Int64(),
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return version, nil
}

// DiscountDetail is a definition together with the version in force.
type DiscountDetail struct {
	Definition *discount.Definition
	// PublishedVersion is the configuration a grant made today would use. It
	// is nil for a definition whose first version is still a draft, which is a
	// normal state for a catalogue entry being prepared, not a fault.
	PublishedVersion *discount.DefinitionVersion
}

// ListDiscounts returns the discount catalogue.
func (s *ConfigService) ListDiscounts(ctx context.Context, actor shared.Actor, activeOnly bool) ([]*discount.Definition, error) {
	return s.deps.Discounts.ListDefinitions(ctx, activeOnly)
}

// GetDiscount returns a definition with the version currently in force.
func (s *ConfigService) GetDiscount(ctx context.Context, actor shared.Actor, definitionID shared.ID) (*DiscountDetail, error) {
	definition, err := s.deps.Discounts.GetDefinition(ctx, definitionID)
	if err != nil {
		return nil, err
	}

	published, err := s.deps.Discounts.GetPublishedVersion(ctx, definition.ID)
	if err != nil {
		if shared.KindOf(err) != shared.KindNotFound {
			return nil, err
		}
		published = nil
	}
	return &DiscountDetail{Definition: definition, PublishedVersion: published}, nil
}

// GetDiscountVersion returns one version, draft or published.
//
// The second pair of eyes needs to read a draft before publishing it, and this
// is how they see the rate they are being asked to put in force.
func (s *ConfigService) GetDiscountVersion(ctx context.Context, actor shared.Actor, versionID shared.ID) (*discount.DefinitionVersion, error) {
	return s.deps.Discounts.GetVersion(ctx, versionID)
}

// nextVersionNo settles the number a new draft version carries.
//
// An explicit number is honoured. Otherwise it follows the version in force,
// which is the only one the repository can see: a collision with a draft in
// between surfaces as discount.duplicate_version, naming the number to use.
func (s *ConfigService) nextVersionNo(ctx context.Context, definitionID shared.ID, requested int32) (int32, error) {
	if requested >= 1 {
		return requested, nil
	}
	current, err := s.deps.Discounts.GetPublishedVersion(ctx, definitionID)
	if err != nil {
		if shared.KindOf(err) != shared.KindNotFound {
			return 0, err
		}
		return 1, nil
	}
	return current.VersionNo + 1, nil
}

// normaliseComponentCodes upper-cases and validates the component codes a
// discount is narrowed to, so a typo is refused here rather than quietly
// matching nothing when an account is priced.
func (s *ConfigService) normaliseComponentCodes(codes []string) ([]string, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	out := make([]string, 0, len(codes))
	for _, raw := range codes {
		code := strings.ToUpper(strings.TrimSpace(raw))
		if !configCodePattern.MatchString(code) {
			return nil, shared.Validation("discount.invalid_component_code",
				"%q is not a fee component code", raw).
				WithDetail("component_code", raw)
		}
		out = append(out, code)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Reference data
// ---------------------------------------------------------------------------

// CreateCollegeInput describes a new faculty.
type CreateCollegeInput struct {
	Code   string
	NameAr string
	NameEn *string
}

// CreateCollege adds a faculty.
func (s *ConfigService) CreateCollege(ctx context.Context, actor shared.Actor, in CreateCollegeInput) (*academic.College, error) {
	var college *academic.College
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		college, err = academic.NewCollege(in.Code, in.NameAr)
		if err != nil {
			return err
		}
		college.NameEn = in.NameEn

		if err := s.deps.Reference.CreateCollege(ctx, college); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "college",
			EntityID:   &college.ID,
			Action:     "reference.college_created",
			Actor:      actor,
			After:      snapshotOf(college),
			Metadata:   map[string]any{"code": college.Code},
		})
	})
	if err != nil {
		return nil, err
	}
	return college, nil
}

// CreateDepartmentInput describes a new programme.
type CreateDepartmentInput struct {
	CollegeID shared.ID
	Code      string
	NameAr    string
	NameEn    *string
	// StageCount is the programme's length in years: six for medicine, four or
	// five for engineering. An enrollment's stage is validated against this
	// rather than against a global maximum.
	StageCount int16
}

// CreateDepartment adds a programme to a college.
func (s *ConfigService) CreateDepartment(ctx context.Context, actor shared.Actor, in CreateDepartmentInput) (*academic.Department, error) {
	var department *academic.Department
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		college, err := s.deps.Reference.GetCollege(ctx, in.CollegeID)
		if err != nil {
			return err
		}

		department, err = academic.NewDepartment(college.ID, in.Code, in.NameAr, in.StageCount)
		if err != nil {
			return err
		}
		department.NameEn = in.NameEn

		if err := s.deps.Reference.CreateDepartment(ctx, department); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "department",
			EntityID:   &department.ID,
			Action:     "reference.department_created",
			Actor:      actor,
			After:      snapshotOf(department),
			Metadata: map[string]any{
				"code":         department.Code,
				"college_code": college.Code,
				"stage_count":  department.StageCount,
			},
		})
	})
	if err != nil {
		return nil, err
	}
	return department, nil
}

// CreateStudyTypeInput describes a new mode of study.
type CreateStudyTypeInput struct {
	Code      string
	NameAr    string
	NameEn    *string
	SortOrder int16
}

// CreateStudyType adds a mode of study.
//
// This command is the proof of a claim the design makes elsewhere: when the
// ministry introduces a mode of study next year, admitting it is data entry
// and not a release. Nothing in the system branches on a study-type code — the
// constants for MORNING, EVENING and PARALLEL exist so that seeded rows can be
// referred to by name, and no rule reads them. A study type created here is
// immediately usable as a fee-policy dimension, an installment-template
// dimension and an enrollment's mode, on exactly the same terms as the three
// that shipped.
func (s *ConfigService) CreateStudyType(ctx context.Context, actor shared.Actor, in CreateStudyTypeInput) (*academic.StudyType, error) {
	var studyType *academic.StudyType
	err := s.deps.Tx.Write(ctx, func(ctx context.Context) error {
		var err error
		studyType, err = academic.NewStudyType(in.Code, in.NameAr, in.SortOrder)
		if err != nil {
			return err
		}
		studyType.NameEn = in.NameEn

		if err := s.deps.Reference.CreateStudyType(ctx, studyType); err != nil {
			return err
		}
		return s.record(ctx, port.AuditEntry{
			EntityType: "study_type",
			EntityID:   &studyType.ID,
			Action:     "reference.study_type_created",
			Actor:      actor,
			After:      snapshotOf(studyType),
			Metadata:   map[string]any{"code": studyType.Code},
		})
	})
	if err != nil {
		return nil, err
	}
	return studyType, nil
}

// ---------------------------------------------------------------------------
// Shared checks
// ---------------------------------------------------------------------------

// requireConfigurableYear refuses to price a year whose books are shut.
//
// Draft and open years are both configurable, and that matters: next year's
// prices are entered months before it opens, which is the whole point of fees
// being data. What is refused is changing what a closed year charges. Accounts
// have already been frozen against it, and new configuration could only
// disagree with them — a correction to a closed year is an adjustment, made
// through the reopening command, not a new price list.
func (s *ConfigService) requireConfigurableYear(year *academic.Year, operation string) error {
	switch year.Status {
	case academic.YearDraft, academic.YearOpen:
		return nil
	}
	return shared.PreconditionFailed("config.year_not_configurable",
		"%s is not allowed: academic year %s is %s", operation, year.Code, year.Status).
		WithDetail("academic_year", year.Code).
		WithDetail("status", string(year.Status)).
		WithDetail("remedy", "configure the year the enrollments belong to, or post an adjustment against the closed one")
}

// checkDepartmentInCollege refuses a scope naming a department that belongs to
// a different college.
//
// The pair is stored denormalised so that resolution stays a single indexed
// lookup, which means nothing in SQL can join the two columns and check them.
// Left unchecked, the row would simply never match any enrollment: the policy
// would look correct in a listing and quietly price nobody.
func (s *ConfigService) checkDepartmentInCollege(ctx context.Context, collegeID, departmentID *shared.ID) error {
	if departmentID == nil {
		return nil
	}
	if collegeID == nil {
		return shared.Validation("config.department_needs_college",
			"a scope naming a department must name its college too")
	}

	department, err := s.deps.Reference.GetDepartment(ctx, *departmentID)
	if err != nil {
		return err
	}
	if department.CollegeID != *collegeID {
		return shared.Validation("config.department_college_mismatch",
			"department %s belongs to a different college than the one named; the scope would match no enrollment",
			department.Code).
			WithDetail("department_code", department.Code).
			WithDetail("department_college_id", department.CollegeID.String()).
			WithDetail("named_college_id", collegeID.String())
	}
	return nil
}

// checkStage keeps a scope's stage inside the range the schema allows.
func (s *ConfigService) checkStage(stage *int16) error {
	if stage == nil {
		return nil
	}
	if *stage < 1 || *stage > 5 {
		return shared.Validation("config.invalid_stage",
			"a stage is between 1 and 5, got %d", *stage).
			WithDetail("stage", int(*stage))
	}
	return nil
}

// boolOr resolves an optional flag against the default the schema uses.
func (s *ConfigService) boolOr(value *bool, fallback bool) bool {
	if value == nil {
		return fallback
	}
	return *value
}
