package httpapi

import "github.com/swibit/flowed/internal/app"

// operationMetadata carries what a route table cannot: what an endpoint is
// for, which roles reach it, and the Go types it binds and returns.
//
// The types are the important half. They are the same structs the handlers use,
// so the generated schema cannot disagree with what the server accepts — add a
// field to a request struct and it appears in the specification, remove one and
// it disappears. The prose is hand-written because nothing can derive it.
//
// Routes absent from this table still appear in the specification with their
// path, method, parameters and error responses; what they lack is a body
// schema. That is a deliberate ordering of work rather than an omission:
// unspecified is honest, and a schema invented to fill the gap would be a
// schema that lies.
var operationMetadata = map[string]operationMeta{
	// ---------------------------------------------------------------- auth
	"POST /api/v1/auth/login": {
		Summary: "Sign in",
		Description: "Exchanges a username and password for an access token and a refresh token. " +
			"Every failure answers identically and takes the same time, so the endpoint cannot be " +
			"used to learn which accounts exist. A cashier must name the desk they are signing in " +
			"at: receipt series run per desk, and a collection with no desk has no paper book to " +
			"reconcile against.",
		Request: LoginRequest{}, Response: TokenResponse{}, Public: true,
	},
	"POST /api/v1/auth/refresh": {
		Summary: "Renew an access token",
		Description: "The user is re-read rather than trusted from the token, and the session " +
			"behind it must still be live — which is what makes signing somebody out take effect " +
			"before their token would have expired.",
		Request: RefreshRequest{}, Response: TokenResponse{}, Public: true,
	},
	"POST /api/v1/auth/logout": {
		Summary:     "Sign out",
		Description: "Ends the session this request arrived on. Idempotent: a retry is a success.",
		Response:    map[string]any{},
	},
	"GET /api/v1/auth/me": {
		Summary: "The signed-in operator",
		Description: "Read from the user row rather than the token, because a client decides its " +
			"whole navigation from this and the token is up to one access lifetime stale.",
		Response: UserDetailView{},
	},
	"POST /api/v1/auth/change-password": {
		Summary: "Change your own password",
		Description: "Available to every account including one that must change its password " +
			"before anything else. The current password is required even though the caller is " +
			"already signed in: an unattended terminal is otherwise a permanent takeover.",
		Request: ChangePasswordRequest{}, Response: map[string]any{},
	},
	"GET /api/v1/auth/sessions":                {Summary: "Your own sign-ins", Response: []SessionView{}},
	"POST /api/v1/auth/sessions/revoke-others": {Summary: "Sign out your other sessions"},

	// ------------------------------------------------------------- students
	"GET /api/v1/students": {
		Summary: "Search students",
		Description: "Matches the student number, the folded name, the folded mother's name and " +
			"the normalised phone. Both sides of every name comparison are folded identically, so " +
			"فاطمه finds فاطمة. Bounded by the caller's organisational scope.",
		Response: []StudentView{},
	},
	"POST /api/v1/students": {
		Summary: "Register a student",
		Description: "Refuses a probable duplicate — same name and mother's name — until somebody " +
			"confirms it is a different person, because splitting one student's payment history " +
			"across two records is painful to unpick later.",
		Request: RegisterStudentRequest{}, Response: StudentView{},
		Roles: []string{"registrar", "admin"},
	},
	"PATCH /api/v1/students/:id/contact": {
		Summary: "Update contact details",
		Description: "The only student fields that change without a version record. Still audited: " +
			"a number changed just before a refund is what an investigation looks for.",
		Request: UpdateContactRequest{}, Response: StudentView{},
	},
	"POST /api/v1/students/:id/identity": {
		Summary: "Record a court-ordered identity change",
		Description: "The previous identity becomes a numbered version rather than being " +
			"overwritten. Iraqi courts change names, and a certificate issued last year must keep " +
			"naming the person as it named them.",
		Request: RecordIdentityChangeRequest{}, Response: StudentView{},
		Roles: []string{"registrar", "admin"},
	},
	"GET /api/v1/students/:id/identity-history": {
		Summary: "Every version of a person's legal identity", Response: []IdentityVersionView{},
	},
	"POST /api/v1/students/:id/merge": {
		Summary: "Merge a duplicate into this record",
		Description: "Enrollments and discount grants move; payments, receipts and adjustments " +
			"never do — re-pointing one would falsify a receipt a student is holding. The source " +
			"record stays as a tombstone so an old student number still resolves.",
		Request: MergeStudentsRequest{}, Response: MergeStudentsResponse{},
		Roles: []string{"registrar", "admin"},
	},

	// ---------------------------------------------------------- enrollments
	"POST /api/v1/enrollments": {
		Summary:     "Enroll a student for a year",
		Description: "Checks the year's debt-block policy and refuses a second live enrollment.",
		Request:     EnrollStudentRequest{}, Response: EnrollmentView{},
	},
	"POST /api/v1/enrollments/:id/status": {
		Summary: "Change an enrollment's status",
		Description: "Deferral, withdrawal, dropout and transfer out each require an explicit " +
			"financial treatment: keep, waive_unpaid, waive_all or partial. There is no default, " +
			"because charging a student who withdrew and waiving what they owe are both defensible " +
			"and only the university can choose. Completing a final-stage enrollment additionally " +
			"decides graduation clearance under the year's policy.",
		Request: ChangeStatusRequest{}, Response: ChangeStatusResponse{},
	},
	"POST /api/v1/enrollments/:id/supersede": {
		Summary: "Replace an enrollment mid-year",
		Description: "The original is superseded rather than edited, its account is settled, and " +
			"the balance moves as a visible pair of transfer adjustments. Payments are never " +
			"re-pointed.",
		Request: SupersedeEnrollmentRequest{}, Response: EnrollmentView{},
	},

	// ------------------------------------------------------------- accounts
	"POST /api/v1/accounts": {
		Summary: "Price an enrollment",
		Description: "Resolves the fee policy, freezes the components as snapshot lines, " +
			"materialises discounts and sponsor commitments, and builds the installment plan. " +
			"Accepts dry_run to return the whole computed outcome while writing nothing.",
		Request: GenerateAccountRequest{}, Idempotent: true,
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/accounts/adjustments": {
		Summary: "Adjust what an account owes",
		Description: "The frozen net never moves. Every later change is a signed row, and what is " +
			"owed is the net plus their sum.",
		Request: PostAdjustmentRequest{}, Idempotent: true,
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/accounts/:id/plan": {
		Summary: "Reschedule or re-split an installment plan",
		Description: "Money already paid is untouchable: an installment carrying an allocation " +
			"keeps its amount and number, only unpaid rows are replaced, and the plan still sums " +
			"to what is owed afterwards. Replaced rows are superseded, never deleted.",
		Request: AdjustPlanRequest{}, Response: AdjustPlanResponse{},
		Roles: []string{"finance_manager", "admin"},
	},
	"GET /api/v1/accounts/:id/plan-revisions": {
		Summary: "Why this schedule changed", Response: []PlanRevisionView{},
	},

	// ------------------------------------------------------------- payments
	"POST /api/v1/payments": {
		Summary: "Collect money",
		Description: "Locks the account, then the year, then the installments, then the receipt " +
			"counter — one order everywhere, so two collections cannot deadlock. Allocates oldest " +
			"due first and turns any excess into credit. Cash requires an open cashier session.",
		Request: RecordPaymentRequest{}, Idempotent: true,
		Roles: []string{"cashier", "finance_manager"},
	},
	"POST /api/v1/voids": {
		Summary: "Request a void",
		Description: "A cashier raises it; a finance manager executes it. Refused outright if the " +
			"payment already carries a posted refund — refunding 400,000 of a million and then " +
			"voiding the whole payment would pay out 1,400,000 against a million received.",
		Request: VoidRequestBody{},
	},
	"POST /api/v1/voids/:id/execute": {
		Summary: "Execute a void", Idempotent: true,
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/refunds": {
		Summary:     "Request a refund",
		Description: "Partial or full. The reversal is scoped to the refunded payment's own allocations.",
		Request:     RefundRequestBody{},
	},
	"POST /api/v1/refunds/:id/post": {
		Summary: "Post an approved refund", Idempotent: true,
		Roles: []string{"finance_manager", "admin"},
	},

	// ---------------------------------------------------------- settlements
	"POST /api/v1/settlements/import": {
		Summary: "Import a bank or card statement",
		Description: "Multipart upload of a CSV. Lines are matched against posted payments by " +
			"external reference; a line matches only when exactly one live payment carries the " +
			"reference at the same amount, and everything else becomes a finding a person works. " +
			"The same file cannot be imported twice.",
		Roles: []string{"finance_manager", "admin"},
	},
	"GET /api/v1/settlements/exceptions": {
		Summary: "Statement lines that did not settle",
		Description: "Money the bank says arrived that the system never recorded, amounts that " +
			"disagree, and references that appear twice. Expected to be worked to empty.",
		Response: []SettlementExceptionView{},
	},
	"GET /api/v1/settlements/unconfirmed": {
		Summary: "Collections no statement has confirmed",
		Description: "The other direction: a receipt was printed and the bank has not said the " +
			"money arrived. The shape of a fraud, and also of an honest typo.",
		Response: []UnconfirmedPaymentView{},
	},
	"POST /api/v1/settlements/lines/:line_id/resolve": {
		Summary: "Resolve a statement line",
		Description: "Matching by hand names the payment; setting a line aside requires a written " +
			"reason. Neither creates money: a collection nobody recorded is recorded through the " +
			"ordinary payment command and matches on the next run.",
		Request: ResolveSettlementLineRequest{}, Response: SettlementLineView{},
	},

	// ------------------------------------------------------ payment intents
	"GET /api/v1/payment-intents/providers": {
		Summary:     "Electronic channels this deployment offers",
		Description: "Empty when the university collects only at the desk.",
		Response:    []PaymentProviderView{},
	},
	"POST /api/v1/payment-intents": {
		Summary: "Begin an electronic collection",
		Description: "Creates the request and hands it to the provider. Nothing here is money: the " +
			"money is the payment row the provider's confirmation creates.",
		Request: InitiatePaymentRequest{}, Response: PaymentIntentView{},
	},
	"POST /api/v1/payment-intents/:id/poll": {
		Summary: "Ask the provider directly",
		Description: "For a collection whose callback never arrived — the ordinary end of a student " +
			"closing the browser. Goes through the same duplicate guard as a callback, so a poll " +
			"and a late callback cannot both post.",
	},
	"POST /webhooks/payments/:provider": {
		Summary: "Provider callback",
		Description: "Unauthenticated by necessity and safe by construction: the body's signature " +
			"is the authority and is verified before any field is read. A duplicate delivery " +
			"answers 200 without posting again; a refused one is recorded, because a burst of them " +
			"is somebody probing.",
		Public: true,
	},

	// --------------------------------------------------------------- portal
	"GET /api/v1/portal/me/statement": {
		Summary: "What I owe",
		Description: "The signed-in student's whole position: every year, what was charged, what " +
			"was paid, what is left, and the soonest unpaid installment. Read from the rows, like " +
			"every figure a person is handed.",
		Response: StudentStatementView{},
	},
	"POST /api/v1/portal/me/statement/verification": {
		Summary: "Mint a verification code for a printed statement",
		Description: "The figures are frozen with the code, so the paper and a later check agree " +
			"even after the student pays something the next morning.",
		Request: IssueVerificationRequest{}, Response: StatementVerificationView{},
	},
	"GET /verify/statement/:code": {
		Summary: "Check a printed statement",
		Description: "Public: an office holding a document a student handed them has no account " +
			"here. The answer carries the name, the number and the figures already on the page — " +
			"not a national identifier, not a telephone number.",
		Response: StatementVerificationAnswer{}, Public: true,
	},
	"POST /api/v1/portal/students/:id/credential": {
		Summary: "Issue a student's portal login",
		Description: "One credential per student, with one role that reaches only their own record. " +
			"The password is generated and shown once.",
		Roles: []string{"registrar", "admin"},
	},

	// -------------------------------------------------------------- sponsors
	"POST /api/v1/sponsors": {
		Summary: "Register a sponsoring body",
		Request: CreateSponsorRequest{}, Response: SponsorView{},
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/sponsorships": {
		Summary: "Record a sponsorship agreement",
		Description: "The settlement mode is required and has no default: `receivable` leaves the " +
			"student liable and treats the sponsor's share as an expected inflow, `covers_debt` " +
			"reduces what the student owes and leaves the university carrying the loss if the " +
			"sponsor defaults. The choice decides who receives a debt letter.",
		Request: CreateSponsorshipRequest{}, Response: SponsorshipView{},
	},
	"GET /api/v1/sponsors/receivables": {
		Summary:     "What each sponsor owes",
		Description: "The invoice list — the thing modelling a sponsorship as a discount made impossible.",
		Response:    []SponsorReceivableView{},
	},

	// ----------------------------------------------------------------- users
	"POST /api/v1/users": {
		Summary: "Create an operator",
		Description: "The password may be omitted, in which case one is generated and returned " +
			"once. Every created account must change its password before any other route answers it.",
		Request: CreateUserRequest{}, Response: CreateUserResponse{},
		Roles: []string{"admin"},
	},
	"POST /api/v1/users/:id/roles": {
		Summary: "Replace an operator's roles",
		Description: "An actor cannot change their own: an administrator is barred from posting " +
			"payments, and without this rule that separation lasts as long as it takes them to " +
			"grant themselves the cashier role. Ends the operator's sessions so the change is immediate.",
		Request: SetRolesRequest{}, Response: UserDetailView{}, Roles: []string{"admin"},
	},
	"POST /api/v1/users/:id/scope": {
		Summary: "Set an operator's organisational scope",
		Description: "`university` reaches every college; `scoped` reaches only the colleges and " +
			"departments granted. A scoped account with no grants reaches nothing, which is the " +
			"safe direction for the absence of a rule to point.",
		Request: SetScopeRequest{}, Response: UserDetailView{}, Roles: []string{"admin"},
	},
	"POST /api/v1/users/:id/disable": {
		Summary:     "Disable an account",
		Description: "Revokes every live session in the same transaction. A reason is required.",
		Request:     ReasonRequest{}, Response: UserDetailView{}, Roles: []string{"admin"},
	},
	"POST /api/v1/users/:id/reset-password": {
		Summary: "Reset somebody's password",
		Description: "The new credential is always must-change, and every session issued under the " +
			"old password ends.",
		Request: ResetPasswordRequest{}, Response: CreateUserResponse{}, Roles: []string{"admin"},
	},
	"GET /api/v1/public/cashier-desks": {
		Summary: "Desks a cashier may sign in at",
		Description: "Public, and it has to be: a cashier cannot obtain a token without naming " +
			"a desk, so a list behind authentication would need a token to see the desks and a " +
			"desk to get a token. Active desks only; the administrative view is authenticated.",
		Response: []CashierDeskView{}, Public: true,
	},
	"GET /api/v1/reconciliation/findings": {
		Summary: "The invariant queue",
		Description: "Open findings, worst and oldest first. `seen_count` is how many nightly " +
			"passes have found the same thing: a number climbing on a finding nobody has taken " +
			"is where to look first.",
		Response: []ReconciliationFindingView{},
		Roles:    []string{"finance_manager", "admin", "auditor"},
	},
	"GET /api/v1/reconciliation/runs": {
		Summary: "Reconciliation passes",
		Description: "Recorded whether or not they found anything: a check that stopped running " +
			"looks exactly like a system with nothing wrong.",
		Response: []ReconciliationRunView{},
		Roles:    []string{"finance_manager", "admin", "auditor"},
	},
	"POST /api/v1/reconciliation/run": {
		Summary:     "Run the checks now",
		Description: "For the moment before closing a year or answering a question about an account.",
		Request:     RunReconciliationRequest{}, Response: RunReconciliationResponse{},
		Roles: []string{"finance_manager", "admin", "auditor"},
	},
	"POST /api/v1/reconciliation/findings/:id/acknowledge": {
		Summary: "Take a finding",
		Description: "Marks it as being worked on without closing it. Two people investigating " +
			"the same drift is waste; a queue where 'somebody is on it' and 'it is fixed' look " +
			"alike closes things nobody fixed.",
		Request: ReasonRequest{}, Response: ReconciliationFindingView{},
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/reconciliation/findings/:id/resolve": {
		Summary: "Close a finding",
		Description: "The resolution text is required and is read by whoever sees the same " +
			"account drift again. Name the command that failed to maintain the cache, or the " +
			"correction posted — never edit a cached total.",
		Request: ResolveFindingRequest{}, Response: ReconciliationFindingView{},
		Roles: []string{"finance_manager", "admin"},
	},
	"POST /api/v1/audit/archive/ship": {
		Summary: "Copy the audit trail off-host now",
		Description: "Ships every entry written since the last block. Scheduled as well; this is " +
			"for the moment before a backup or an investigation, when waiting for the timer is " +
			"the wrong answer.",
		Response: app.ShipResult{}, Roles: []string{"admin"},
	},
	"GET /api/v1/audit/archive/verify": {
		Summary: "Check the off-host copy against the database",
		Description: "The only check in the system that can detect an audit entry having been " +
			"deleted: the hash chain verifies whatever is present, so a removed tail leaves an " +
			"intact chain. Answers 200 with `ok: false` when the two disagree — the request " +
			"succeeded, and what it found is the answer.",
		Response: app.VerifyReport{}, Roles: []string{"admin", "auditor"},
	},
	"GET /api/v1/users/:id/login-history": {
		Summary:     "Sign-in attempts against an account",
		Description: "A burst of failures before a questionable receipt is the shape of somebody borrowing a login.",
		Response:    []LoginAttemptView{},
	},

	// --------------------------------------------------------------- reports
	"GET /api/v1/reports/debt": {
		Summary: "Outstanding balances",
		Description: "Requires a year unless prior_years_only is set: the year filter is what " +
			"bounds the scan, and figures from two years are not comparable anyway.",
	},
	"GET /api/v1/reports/aging":        {Summary: "Receivables by how long they have been owed"},
	"GET /api/v1/reports/installments": {Summary: "Expected against collected, by due month"},
	"GET /api/v1/reports/departments":  {Summary: "A year's money, college then department"},
	"GET /api/v1/reports/cashier-daily": {
		Summary: "Cash movement per cashier and day",
		Description: "Takes a date range rather than a year: a shift belongs to a day. A void on " +
			"the same day clears from the drawer; across days it is a refund.",
	},

	// ------------------------------------------------------------- oversight
	"GET /api/v1/oversight/audit/verify": {
		Summary: "Verify the audit hash chain",
		Description: "Empty means every entry still hashes to what its successor recorded. A " +
			"non-empty result names where the chain breaks.",
	},
	"GET /api/v1/oversight/reconciliation": {
		Summary:     "Accounts whose cached totals disagree with their transactions",
		Description: "Expected to be empty. The financial year close refuses while it is not.",
	},

	// -------------------------------------------------------------- service
	"GET /health": {Summary: "Liveness", Description: "Answers without touching the database.", Public: true},
	"GET /ready": {
		Summary:     "Readiness",
		Description: "Reports the database and the pool. A replica that cannot reach PostgreSQL should leave rotation.",
		Public:      true,
	},
}
