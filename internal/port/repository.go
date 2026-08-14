// Package port declares the interfaces the application layer depends on.
//
// Everything here is phrased in domain terms. There is no SQL, no pgx type,
// and no HTTP concept: the application services are written against these, and
// the PostgreSQL adapter is one implementation of them. That boundary is what
// keeps the financial rules testable without a database and what would make a
// second read model — a reporting replica, say — an additive change.
package port

import (
	"context"
	"time"

	"github.com/swibit/flowed/internal/domain/academic"
	"github.com/swibit/flowed/internal/domain/billing"
	"github.com/swibit/flowed/internal/domain/discount"
	"github.com/swibit/flowed/internal/domain/money"
	"github.com/swibit/flowed/internal/domain/payment"
	"github.com/swibit/flowed/internal/domain/shared"
	"github.com/swibit/flowed/internal/domain/student"
)

// TxManager owns the transaction boundary.
//
// A command handler wraps its whole body in Write; every repository call made
// inside that closure joins the same transaction, without the handler passing
// a transaction handle around. Nesting is supported through savepoints, so a
// bulk command can call a single-item command per row and have one row's
// failure roll back only that row.
type TxManager interface {
	// Write runs fn in a read-write transaction, retrying automatically on
	// serialization failure and deadlock.
	Write(ctx context.Context, fn func(ctx context.Context) error) error
	// Read runs fn in a read-only transaction, for a report that must see one
	// consistent snapshot across several queries.
	Read(ctx context.Context, fn func(ctx context.Context) error) error
	// RequireTx returns an error unless a transaction is already open. Money
	// paths assert this so a missing boundary fails loudly instead of writing
	// partial state.
	RequireTx(ctx context.Context, operation string) error
}

// ---------------------------------------------------------------------------
// Students
// ---------------------------------------------------------------------------

// StudentSearch describes a query against the student index.
type StudentSearch struct {
	// Query is matched against the student number, the folded name, the folded
	// mother's name, and the normalised phone.
	Query          string
	AcademicYearID *shared.ID
	DepartmentID   *shared.ID
	CollegeID      *shared.ID
	StudyTypeID    *shared.ID
	Stage          *int16
	Status         *student.Status
	// OnlyWithDebt narrows to students with an outstanding balance.
	OnlyWithDebt bool
	// Scope restricts the result to the colleges and departments the caller may
	// see. Applied in the query rather than by filtering afterwards: the point
	// of a scope is not to read the other colleges' rows at all.
	Scope  shared.ScopeFilter
	Limit  int
	Offset int
}

// StudentRepository stores student identity.
type StudentRepository interface {
	Create(ctx context.Context, s *student.Student) error
	Update(ctx context.Context, s *student.Student) error
	GetByID(ctx context.Context, id shared.ID) (*student.Student, error)
	GetByStudentNo(ctx context.Context, studentNo string) (*student.Student, error)
	// FindPossibleDuplicates looks for an existing person before a new record
	// is created. In Iraq the discriminator is the mother's name, which is why
	// it is a first-class search field rather than an afterthought.
	FindPossibleDuplicates(ctx context.Context, fullName, motherName string, birthDate *shared.Date) ([]*student.Student, error)
	Search(ctx context.Context, q StudentSearch) ([]*student.Student, int, error)
	AppendIdentityVersion(ctx context.Context, v *student.IdentityVersion) error
	IdentityHistory(ctx context.Context, studentID shared.ID) ([]*student.IdentityVersion, error)
}

// ---------------------------------------------------------------------------
// Academic
// ---------------------------------------------------------------------------

// AcademicYearRepository stores academic years.
type AcademicYearRepository interface {
	Create(ctx context.Context, y *academic.Year) error
	Update(ctx context.Context, y *academic.Year) error
	GetByID(ctx context.Context, id shared.ID) (*academic.Year, error)
	GetByCode(ctx context.Context, code string) (*academic.Year, error)
	List(ctx context.Context) ([]*academic.Year, error)
	// GetForUpdate locks the year row. Every money command takes this lock
	// before posting, so a year cannot close underneath an in-flight payment:
	// without it, a payment reading "open" under MVCC can commit into a year
	// that closed a millisecond later.
	GetForUpdate(ctx context.Context, id shared.ID) (*academic.Year, error)
	// CurrentOpen returns the years currently accepting money. Two are
	// legitimately open each autumn while results close one and registration
	// opens the next.
	CurrentOpen(ctx context.Context) ([]*academic.Year, error)
}

// EnrollmentFilter narrows an enrollment query.
type EnrollmentFilter struct {
	// Scope restricts the result to the caller's colleges and departments.
	Scope          shared.ScopeFilter
	AcademicYearID *shared.ID
	StudentID      *shared.ID
	CollegeID      *shared.ID
	DepartmentID   *shared.ID
	StudyTypeID    *shared.ID
	Stage          *int16
	Status         *academic.EnrollmentStatus
	Result         *academic.AcademicResult
	// ExcludeSuperseded restricts the result to real seats, which is what head
	// counts must use.
	ExcludeSuperseded bool
	Limit             int
	Offset            int
}

// EnrollmentRepository stores enrollments.
type EnrollmentRepository interface {
	Create(ctx context.Context, e *academic.Enrollment) error
	Update(ctx context.Context, e *academic.Enrollment) error
	GetByID(ctx context.Context, id shared.ID) (*academic.Enrollment, error)
	// GetLive returns the student's live enrollment for a year, if any. At most
	// one can exist; the database enforces it with a partial unique index.
	GetLive(ctx context.Context, studentID, yearID shared.ID) (*academic.Enrollment, error)
	// History returns every enrollment for a student across all years, oldest
	// first, including superseded rows so the full lineage is visible.
	History(ctx context.Context, studentID shared.ID) ([]*academic.Enrollment, error)
	List(ctx context.Context, f EnrollmentFilter) ([]*academic.Enrollment, int, error)
	// CountAttempts returns how many counted attempts a student has made at a
	// stage in a department, which validates the attempt number rather than
	// trusting what a caller supplies.
	CountAttempts(ctx context.Context, studentID, departmentID shared.ID, stage int16) (int, error)
	// NextSequenceNo returns the next supersede-chain position for a student
	// in a year.
	NextSequenceNo(ctx context.Context, studentID, yearID shared.ID) (int16, error)

	// Reassign moves an enrollment to another student. Used by the merge
	// command and by nothing else: it is the one operation that changes whose
	// enrollment a row is, and it exists because a duplicate person record
	// splits one student's history across two identities.
	Reassign(ctx context.Context, enrollmentID, toStudentID shared.ID) error
	// PendingResults counts enrollments in a year with no recorded outcome,
	// which gates the academic close.
	PendingResults(ctx context.Context, yearID shared.ID) (int, error)
	CreateHostingRecord(ctx context.Context, h *academic.HostingRecord) error
	GetHostingRecord(ctx context.Context, enrollmentID shared.ID) (*academic.HostingRecord, error)
	UpdateHostingRecord(ctx context.Context, h *academic.HostingRecord) error
	ListHostingRecords(ctx context.Context, yearID *shared.ID, direction *academic.HostingDirection) ([]*academic.HostingRecord, error)
}

// ---------------------------------------------------------------------------
// Reference data
// ---------------------------------------------------------------------------

// MasterDataKind names a table whose usage the reference repository can count.
//
// Used by the guards that decide whether a field may still be edited: a payment
// method cannot stop being cash once cash has been taken through it.
type MasterDataKind string

const (
	MasterCollege         MasterDataKind = "college"
	MasterDepartment      MasterDataKind = "department"
	MasterStudyType       MasterDataKind = "study_type"
	MasterStudentCategory MasterDataKind = "student_category"
	MasterPaymentMethod   MasterDataKind = "payment_method"
	MasterCashierDesk     MasterDataKind = "cashier_desk"
)

// College, Department, StudyType and StudentCategory are configuration the
// administration owns. They are grouped into one repository because they are
// always read together when validating an enrollment's context.
type ReferenceRepository interface {
	ListColleges(ctx context.Context, activeOnly bool) ([]*academic.College, error)
	GetCollege(ctx context.Context, id shared.ID) (*academic.College, error)
	CreateCollege(ctx context.Context, c *academic.College) error

	ListDepartments(ctx context.Context, collegeID *shared.ID, activeOnly bool) ([]*academic.Department, error)
	GetDepartment(ctx context.Context, id shared.ID) (*academic.Department, error)
	CreateDepartment(ctx context.Context, d *academic.Department) error

	ListStudyTypes(ctx context.Context, activeOnly bool) ([]*academic.StudyType, error)
	GetStudyType(ctx context.Context, id shared.ID) (*academic.StudyType, error)
	GetStudyTypeByCode(ctx context.Context, code string) (*academic.StudyType, error)
	CreateStudyType(ctx context.Context, s *academic.StudyType) error

	ListStudentCategories(ctx context.Context, activeOnly bool) ([]*academic.StudentCategory, error)
	GetStudentCategoryByCode(ctx context.Context, code string) (*academic.StudentCategory, error)

	ListPaymentMethods(ctx context.Context, activeOnly bool) ([]*payment.Method, error)
	GetPaymentMethod(ctx context.Context, id shared.ID) (*payment.Method, error)

	// Updates. Master data is edited in place — a college renamed is the same
	// college — but never deleted: a row referenced by an enrollment from 2019
	// cannot go away, and the retirement flag is what takes it out of use.
	UpdateCollege(ctx context.Context, c *academic.College) error
	UpdateDepartment(ctx context.Context, d *academic.Department) error
	UpdateStudyType(ctx context.Context, s *academic.StudyType) error

	CreateStudentCategory(ctx context.Context, c *academic.StudentCategory) error
	GetStudentCategory(ctx context.Context, id shared.ID) (*academic.StudentCategory, error)
	UpdateStudentCategory(ctx context.Context, c *academic.StudentCategory) error

	CreatePaymentMethod(ctx context.Context, m *payment.Method) error
	UpdatePaymentMethod(ctx context.Context, m *payment.Method) error

	ListCashierDesks(ctx context.Context, activeOnly bool) ([]*payment.CashierDesk, error)
	GetCashierDesk(ctx context.Context, id shared.ID) (*payment.CashierDesk, error)
	CreateCashierDesk(ctx context.Context, d *payment.CashierDesk) error
	UpdateCashierDesk(ctx context.Context, d *payment.CashierDesk) error

	// UsageCount reports how many rows depend on a piece of master data.
	//
	// The guard behind every edit that could rewrite history: a payment method
	// cannot stop being cash once cash has been taken through it, and a
	// department cannot shrink below the stages students are actually
	// registered in. Nothing else can answer that question — the domain has no
	// view of how widely a row is referenced.
	UsageCount(ctx context.Context, kind MasterDataKind, id shared.ID) (int, error)
	// HighestStageInUse bounds how far a department's stage count may shrink.
	HighestStageInUse(ctx context.Context, departmentID shared.ID) (int16, error)
}

// ---------------------------------------------------------------------------
// Fees
// ---------------------------------------------------------------------------

// FeeScope is the set of dimensions a fee policy resolves against.
type FeeScope struct {
	AcademicYearID    shared.ID
	CollegeID         shared.ID
	DepartmentID      shared.ID
	Stage             int16
	StudyTypeID       shared.ID
	StudentCategoryID shared.ID
}

// FeePolicyRepository stores fee configuration.
type FeePolicyRepository interface {
	Create(ctx context.Context, p *billing.FeePolicy) error
	Publish(ctx context.Context, policyID shared.ID, actor shared.ID, at time.Time) error
	GetByID(ctx context.Context, id shared.ID) (*billing.FeePolicy, error)
	List(ctx context.Context, yearID shared.ID) ([]*billing.FeePolicy, error)
	// Resolve returns the single published policy that best matches the scope,
	// chosen by the highest specificity score. It returns a not-found error
	// when nothing matches: an unpriced enrollment must stop the operation,
	// never default to a zero fee.
	Resolve(ctx context.Context, scope FeeScope) (*billing.FeePolicy, error)
}

// InstallmentTemplateRepository stores installment plan templates.
type InstallmentTemplateRepository interface {
	Create(ctx context.Context, t *billing.InstallmentTemplate) error
	Publish(ctx context.Context, templateID shared.ID, actor shared.ID, at time.Time) error
	GetByID(ctx context.Context, id shared.ID) (*billing.InstallmentTemplate, error)
	List(ctx context.Context, yearID *shared.ID) ([]*billing.InstallmentTemplate, error)
	Resolve(ctx context.Context, scope FeeScope) (*billing.InstallmentTemplate, error)
}

// ---------------------------------------------------------------------------
// Discounts
// ---------------------------------------------------------------------------

// DiscountRepository stores discount configuration and grants.
type DiscountRepository interface {
	CreateDefinition(ctx context.Context, d *discount.Definition) error
	GetDefinition(ctx context.Context, id shared.ID) (*discount.Definition, error)
	GetDefinitionByCode(ctx context.Context, code string) (*discount.Definition, error)
	ListDefinitions(ctx context.Context, activeOnly bool) ([]*discount.Definition, error)

	CreateVersion(ctx context.Context, v *discount.DefinitionVersion) error
	PublishVersion(ctx context.Context, versionID, actor shared.ID, at time.Time) error
	GetVersion(ctx context.Context, id shared.ID) (*discount.DefinitionVersion, error)
	// GetPublishedVersion returns the version currently in force for a
	// definition. An application freezes the id it returns, so a later
	// publication cannot reach back into a computed amount.
	GetPublishedVersion(ctx context.Context, definitionID shared.ID) (*discount.DefinitionVersion, error)

	CreateAssignment(ctx context.Context, a *discount.Assignment) error
	UpdateAssignment(ctx context.Context, a *discount.Assignment) error
	GetAssignment(ctx context.Context, id shared.ID) (*discount.Assignment, error)
	ListAssignmentsForStudent(ctx context.Context, studentID shared.ID) ([]*discount.Assignment, error)
	// ApprovedAssignmentsCovering returns the grants whose scope includes a
	// year, which is what account generation materialises from.
	ApprovedAssignmentsCovering(ctx context.Context, studentID shared.ID, yearCode string) ([]*discount.Assignment, error)
	// HasOverlappingAssignment guards against granting the same discount twice
	// over intersecting years.
	HasOverlappingAssignment(ctx context.Context, studentID, definitionID shared.ID, fromCode, toCode string, excluding *shared.ID) (bool, error)

	CreateApplication(ctx context.Context, a *discount.Application) error
	UpdateApplication(ctx context.Context, a *discount.Application) error
	GetApplication(ctx context.Context, id shared.ID) (*discount.Application, error)
	ListApplications(ctx context.Context, accountID shared.ID) ([]*discount.Application, error)
}

// ---------------------------------------------------------------------------
// Financial accounts
// ---------------------------------------------------------------------------

// AccountRepository stores financial accounts and everything frozen into them.
type AccountRepository interface {
	Create(ctx context.Context, a *billing.Account, snapshot []*billing.SnapshotLine) error
	Update(ctx context.Context, a *billing.Account) error
	GetByID(ctx context.Context, id shared.ID) (*billing.Account, error)
	GetByEnrollment(ctx context.Context, enrollmentID shared.ID) (*billing.Account, error)
	// GetForUpdate locks the account row.
	//
	// This is the single most important call in the system. Every command that
	// moves money takes it first, which serialises two cashiers working on the
	// same student and makes the cached totals safe to maintain in-transaction.
	// The lock order is fixed everywhere — account, then year, then number
	// series — so concurrent commands cannot deadlock against each other.
	GetForUpdate(ctx context.Context, id shared.ID) (*billing.Account, error)
	ListForStudent(ctx context.Context, studentID shared.ID) ([]*billing.Account, error)
	Snapshot(ctx context.Context, accountID shared.ID) ([]*billing.SnapshotLine, error)

	CreateAdjustment(ctx context.Context, adj *billing.Adjustment) error
	ListAdjustments(ctx context.Context, accountID shared.ID) ([]*billing.Adjustment, error)

	CreateCredit(ctx context.Context, c *billing.CreditEntry) error
	// GetCreditForUpdate locks a credit row before it is spent. Carrying credit
	// forward and refunding it in cash are separate paths that lock different
	// accounts; without a lock on the credit itself, both could read the same
	// balance and pay it out twice.
	GetCreditForUpdate(ctx context.Context, id shared.ID) (*billing.CreditEntry, error)
	ListOpenCredits(ctx context.Context, studentID shared.ID) ([]*billing.CreditEntry, error)
	UpdateCredit(ctx context.Context, c *billing.CreditEntry) error
	RecordCreditConsumption(ctx context.Context, c *billing.CreditConsumption) error

	// OutstandingForStudent totals what a student owes across every year, used
	// by the registration debt check.
	OutstandingForStudent(ctx context.Context, studentID shared.ID, excludingYear *shared.ID) (money.Amount, error)
	// DraftPaymentCount gates the financial close of a year.
	DraftPaymentCount(ctx context.Context, yearID shared.ID) (int, error)
	// ReconciliationDrift lists accounts whose caches disagree with their
	// transaction rows. Expected to be empty.
	ReconciliationDrift(ctx context.Context, limit int) ([]ReconciliationRow, error)
}

// ReconciliationRow reports one account whose cached totals drifted.
type ReconciliationRow struct {
	AccountID        shared.ID
	CachedPaid       money.Amount
	ComputedPaid     money.Amount
	CachedRefunded   money.Amount
	ComputedRefunded money.Amount
	CachedAdjustment money.Amount
	ComputedAdjust   money.Amount
	CachedCredit     money.Amount
	ComputedCredit   money.Amount
}

// InstallmentRepository stores installment plans.
type InstallmentRepository interface {
	CreatePlan(ctx context.Context, installments []*billing.Installment) error
	Update(ctx context.Context, i *billing.Installment) error
	GetByID(ctx context.Context, id shared.ID) (*billing.Installment, error)
	ListForAccount(ctx context.Context, accountID shared.ID) ([]*billing.Installment, error)
	// ListOpenForUpdate returns the account's unsettled installments, locked in
	// a deterministic order so two concurrent payments cannot deadlock.
	ListOpenForUpdate(ctx context.Context, accountID shared.ID) ([]*billing.Installment, error)
	SupersedePlan(ctx context.Context, accountID shared.ID, replacedBy map[shared.ID]shared.ID) error
}

// ---------------------------------------------------------------------------
// Payments
// ---------------------------------------------------------------------------

// PaymentRepository stores payments and their allocations.
type PaymentRepository interface {
	Create(ctx context.Context, p *payment.Payment, allocations []*payment.Allocation) error
	Update(ctx context.Context, p *payment.Payment) error
	GetByID(ctx context.Context, id shared.ID) (*payment.Payment, error)
	GetByIdempotencyKey(ctx context.Context, key string) (*payment.Payment, error)
	GetByReceiptNo(ctx context.Context, seriesID shared.ID, receiptNo string) (*payment.Payment, error)
	ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Payment, error)
	ListForStudent(ctx context.Context, studentID shared.ID) ([]*payment.Payment, error)
	// LiveAllocations returns a payment's allocations that have not been
	// reversed. A refund plan is built only from these, which is what stops
	// one payment's refund unwinding another payment's funding.
	LiveAllocations(ctx context.Context, paymentID shared.ID) ([]*billing.ExistingAllocation, error)
	CreateAllocations(ctx context.Context, allocations []*payment.Allocation) error
	// FindNearDuplicate looks for a payment with the same account, amount and
	// method within a short window, catching a retry from a client that lost
	// its idempotency key across a restart.
	FindNearDuplicate(ctx context.Context, accountID shared.ID, amount money.Amount, methodID shared.ID, within time.Duration) (*payment.Payment, error)
	// PostedRefundTotal is what has already been returned against a payment.
	PostedRefundTotal(ctx context.Context, paymentID shared.ID) (money.Amount, error)
	CountPostedRefunds(ctx context.Context, paymentID shared.ID) (int, error)
}

// RefundRepository stores refunds.
type RefundRepository interface {
	Create(ctx context.Context, r *payment.Refund) error
	Update(ctx context.Context, r *payment.Refund) error
	GetByID(ctx context.Context, id shared.ID) (*payment.Refund, error)
	GetForUpdate(ctx context.Context, id shared.ID) (*payment.Refund, error)
	ListForPayment(ctx context.Context, paymentID shared.ID) ([]*payment.Refund, error)
	ListForAccount(ctx context.Context, accountID shared.ID) ([]*payment.Refund, error)
	ListPending(ctx context.Context) ([]*payment.Refund, error)
	CreateAllocations(ctx context.Context, allocations []*payment.RefundAllocation) error
}

// VoidRequestRepository stores void requests, the document that makes a void a
// two-person act rather than a permission flag.
type VoidRequestRepository interface {
	Create(ctx context.Context, r *payment.VoidRequest) error
	Update(ctx context.Context, r *payment.VoidRequest) error
	GetByID(ctx context.Context, id shared.ID) (*payment.VoidRequest, error)
	ListPending(ctx context.Context) ([]*payment.VoidRequest, error)
}

// NumberSeriesRepository allocates receipt numbers.
type NumberSeriesRepository interface {
	// NextNumber increments the counter under a row lock and returns the
	// formatted number. Called inside the posting transaction, so a rolled-back
	// payment returns its number rather than burning it.
	//
	// The counter is locked last in the command's lock order, after the account
	// and the year, so that a hot series cannot deadlock with an account lock.
	NextNumber(ctx context.Context, kind payment.SeriesKind, yearID shared.ID, deskID *shared.ID) (string, shared.ID, error)
	EnsureSeries(ctx context.Context, kind payment.SeriesKind, yearID shared.ID, deskID *shared.ID, prefix string) (shared.ID, error)
}

// CashierSessionRepository stores cashier shifts.
type CashierSessionRepository interface {
	Open(ctx context.Context, s *payment.CashierSession) error
	Close(ctx context.Context, s *payment.CashierSession) error
	Update(ctx context.Context, s *payment.CashierSession) error
	GetByID(ctx context.Context, id shared.ID) (*payment.CashierSession, error)
	GetOpenForUser(ctx context.Context, userID shared.ID) (*payment.CashierSession, error)
	// ExpectedCash totals what the drawer should hold: the opening float plus
	// cash collected, less cash refunded and cash returned on voids.
	ExpectedCash(ctx context.Context, sessionID shared.ID) (money.Amount, error)
}

// ---------------------------------------------------------------------------
// Cross-cutting
// ---------------------------------------------------------------------------

// GraduationClearance is one clearance decision (براءة الذمة), including the
// ones that refused.
type GraduationClearance struct {
	ID             shared.ID
	StudentID      shared.ID
	EnrollmentID   shared.ID
	AcademicYearID shared.ID
	Outstanding    money.Amount
	Policy         academic.ClearancePolicy
	Cleared        bool
	OverrideReason *string
	OverrideBy     *shared.ID
	DecidedAt      time.Time
	DecidedBy      *shared.ID
}

// PlanRevision records why an installment plan changed.
type PlanRevision struct {
	ID                 shared.ID
	AccountID          shared.ID
	PlanVersion        int16
	Kind               string
	Reason             string
	InstallmentsBefore int16
	InstallmentsAfter  int16
	UnpaidBefore       money.Amount
	UnpaidAfter        money.Amount
	ApprovedBy         *shared.ID
	CreatedAt          time.Time
	CreatedBy          *shared.ID
}

// StudentMerge records one duplicate folded into a canonical record.
type StudentMerge struct {
	ID               shared.ID
	SourceID         shared.ID
	TargetID         shared.ID
	Reason           string
	EnrollmentsMoved int16
	AccountsMoved    int16
	DiscountsMoved   int16
	MergedAt         time.Time
	MergedBy         *shared.ID
}

// LifecycleRepository stores the records of the decisions that end or reshape a
// student's relationship with the university.
//
// Grouped in one interface because they share a property: each is append-only,
// each exists to make a decision answerable years later, and none of them is
// read on a hot path.
type LifecycleRepository interface {
	RecordClearance(ctx context.Context, c *GraduationClearance) error
	ListClearances(ctx context.Context, studentID shared.ID) ([]*GraduationClearance, error)
	LatestClearance(ctx context.Context, enrollmentID shared.ID) (*GraduationClearance, error)

	RecordPlanRevision(ctx context.Context, r *PlanRevision) error
	ListPlanRevisions(ctx context.Context, accountID shared.ID) ([]*PlanRevision, error)

	RecordMerge(ctx context.Context, m *StudentMerge) error
	ListMerges(ctx context.Context, targetID shared.ID) ([]*StudentMerge, error)
	MergeOf(ctx context.Context, sourceID shared.ID) (*StudentMerge, error)
}

// AuditEntry is one record of who changed what.
type AuditEntry struct {
	ID             shared.ID
	EntityType     string
	EntityID       *shared.ID
	Action         string
	Actor          shared.Actor
	OccurredAt     time.Time
	Before         any
	After          any
	Metadata       map[string]any
	Reason         *string
	AcademicYearID *shared.ID
	StudentID      *shared.ID
	AccountID      *shared.ID
	RequestID      string
}

// AuditRepository appends to the audit trail.
//
// There is no update and no delete, and the database refuses both. Entries are
// hash-chained on insert, so an entry removed or altered later breaks
// verification of every entry after it.
type AuditRepository interface {
	Append(ctx context.Context, entry AuditEntry) error
	List(ctx context.Context, entityType string, entityID shared.ID, limit int) ([]AuditEntry, error)
	ListForStudent(ctx context.Context, studentID shared.ID, limit int) ([]AuditEntry, error)
	VerifyChain(ctx context.Context, fromSequence int64) ([]ChainProblem, error)
}

// ChainProblem reports an audit entry that failed verification.
type ChainProblem struct {
	SequenceNo int64
	EntryID    shared.ID
	OccurredAt time.Time
	Problem    string
}

// IdempotencyRepository remembers commands already executed so a retry returns
// the original outcome rather than performing the work twice.
type IdempotencyRepository interface {
	// Begin claims a key. It returns the stored record when the key was already
	// used, in which case the caller replays that outcome instead of acting.
	Begin(ctx context.Context, key, commandName, payloadHash string, actor *shared.ID) (existing *IdempotencyRecord, err error)
	Complete(ctx context.Context, key, commandName string, status int, body []byte) error
	Fail(ctx context.Context, key, commandName, errorCode string) error
	PurgeExpired(ctx context.Context, before time.Time) (int64, error)
}

// IdempotencyRecord is a previously executed command.
type IdempotencyRecord struct {
	Key          string
	CommandName  string
	PayloadHash  string
	Status       string
	ResponseCode int
	ResponseBody []byte
	ErrorCode    string
	CreatedAt    time.Time
	CompletedAt  *time.Time
}

// UserRepository stores application users, their roles and their scope.
type UserRepository interface {
	Create(ctx context.Context, u *User) error
	Update(ctx context.Context, u *User) error
	GetByID(ctx context.Context, id shared.ID) (*User, error)
	GetByUsername(ctx context.Context, username string) (*User, error)
	List(ctx context.Context, activeOnly bool) ([]*User, error)
	SetRoles(ctx context.Context, userID shared.ID, roles []shared.Role, grantedBy shared.ID) error
	RecordLogin(ctx context.Context, userID shared.ID, at time.Time) error

	// SetScope replaces a user's organisational grants wholesale, for the same
	// reason SetRoles replaces rather than merges: an administrator removing a
	// college expects it gone.
	SetScope(ctx context.Context, userID shared.ID, mode shared.ScopeMode, colleges, departments []shared.ID, grantedBy shared.ID) error

	// RecordFailedLogin increments the failure counter and returns the value
	// after the increment, so the caller can decide on a lockout without a
	// second read that another attempt could interleave with.
	RecordFailedLogin(ctx context.Context, userID shared.ID, at time.Time) (int, error)
	// ClearLoginFailures resets the counter and any lockout after a success.
	ClearLoginFailures(ctx context.Context, userID shared.ID) error
	// LockAccount refuses sign-in until the given moment.
	LockAccount(ctx context.Context, userID shared.ID, until time.Time) error
	// SetPassword stores a new hash. mustChange marks a credential that
	// authenticates and nothing else until its holder replaces it.
	SetPassword(ctx context.Context, userID shared.ID, hash string, mustChange bool, at time.Time) error
	// SetActive enables or disables an account, recording who and why.
	SetActive(ctx context.Context, userID shared.ID, active bool, by shared.ID, reason *string, at time.Time) error
}

// User is an operator of the system.
type User struct {
	ID           shared.ID
	Username     string
	FullName     string
	PasswordHash string
	Email        *string
	Roles        []shared.Role
	IsActive     bool
	LastLoginAt  *time.Time
	CreatedAt    time.Time
	UpdatedAt    time.Time

	// MustChangePassword marks a reset credential. Every route except the
	// password change refuses while it is set.
	MustChangePassword bool
	PasswordChangedAt  *time.Time

	// FailedLoginCount and LockedUntil implement per-account throttling. The
	// IP limiter cannot do this: it sees a campus NAT shared by a hall of
	// terminals, so a budget large enough for the hall is large enough to
	// guess one cashier's password all afternoon.
	FailedLoginCount int
	LastFailedLogin  *time.Time
	LockedUntil      *time.Time

	DisabledAt     *time.Time
	DisabledBy     *shared.ID
	DisabledReason *string

	// ScopeMode and the two lists give the actor its organisational reach.
	ScopeMode   shared.ScopeMode
	Colleges    []shared.ID
	Departments []shared.ID
}

// Scope renders the user's organisational reach for an actor.
func (u *User) Scope() shared.Scope {
	mode := u.ScopeMode
	if mode == "" {
		mode = shared.ScopeUniversity
	}
	return shared.Scope{Mode: mode, Colleges: u.Colleges, Departments: u.Departments}
}

// IsLocked reports whether per-account throttling currently refuses sign-in.
func (u *User) IsLocked(now time.Time) bool {
	return u.LockedUntil != nil && u.LockedUntil.After(now)
}

// Session is one sign-in. Revoking the row is what makes logout, "sign out my
// other sessions" and disabling an account take effect before the access token
// would have expired on its own.
type Session struct {
	ID            shared.ID
	UserID        shared.ID
	IssuedAt      time.Time
	ExpiresAt     time.Time
	LastSeenAt    time.Time
	RevokedAt     *time.Time
	RevokedBy     *shared.ID
	RevokedReason *string
	IPAddress     *string
	UserAgent     *string
	CashierDeskID *shared.ID
}

// Active reports whether the session may still be used.
func (s *Session) Active(now time.Time) bool {
	return s.RevokedAt == nil && s.ExpiresAt.After(now)
}

// SessionRepository stores sign-ins so that a credential can be withdrawn.
type SessionRepository interface {
	Create(ctx context.Context, s *Session) error
	GetByID(ctx context.Context, id shared.ID) (*Session, error)
	ListForUser(ctx context.Context, userID shared.ID, includeEnded bool) ([]*Session, error)
	// Revoke ends one session. Idempotent: revoking an already-revoked session
	// is a success, because a client retrying a logout must not see an error.
	Revoke(ctx context.Context, id shared.ID, by shared.ID, reason string, at time.Time) error
	// RevokeAllForUser ends every live session, optionally sparing the one the
	// request arrived on so "sign out my other sessions" does not sign the
	// operator out mid-task.
	RevokeAllForUser(ctx context.Context, userID shared.ID, except *shared.ID, by shared.ID, reason string, at time.Time) (int, error)
	// Touch records that a session was seen, so an operator listing their
	// sessions can tell a live desk from one left open in a browser weeks ago.
	Touch(ctx context.Context, id shared.ID, at time.Time) error
	// PurgeExpired deletes sessions past their expiry. They are not an audit
	// trail; the audit log is.
	PurgeExpired(ctx context.Context, before time.Time) (int64, error)
}

// LoginAttempt is one sign-in attempt, successful or not.
type LoginAttempt struct {
	ID          shared.ID
	Username    string
	UserID      *shared.ID
	Succeeded   bool
	FailureCode *string
	IPAddress   *string
	UserAgent   *string
	OccurredAt  time.Time
}

// LoginAttemptRepository records sign-in attempts for throttling and for the
// security trail.
type LoginAttemptRepository interface {
	Record(ctx context.Context, a LoginAttempt) error
	// CountRecentFailures counts failures against a username inside a window,
	// which throttles an attacker cycling through usernames that do not exist
	// — those have no user row to hold a counter.
	CountRecentFailures(ctx context.Context, username string, since time.Time) (int, error)
	ListForUser(ctx context.Context, userID shared.ID, limit int) ([]LoginAttempt, error)
	PurgeBefore(ctx context.Context, before time.Time) (int64, error)
}

// HasRole reports whether the user holds a role.
func (u *User) HasRole(role shared.Role) bool {
	for _, r := range u.Roles {
		if r == role {
			return true
		}
	}
	return false
}
