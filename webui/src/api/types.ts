/**
 * The wire contract, transcribed from internal/adapter/httpapi/dto.go.
 *
 * Money arrives as a JSON number of whole dinars and is declared here as
 * `RawAmount` rather than `number`, so that a screen cannot render one by
 * accident: everything that displays money goes through money.amount() first.
 * That is the only guard in a TypeScript client that actually catches the
 * mistake §13 is worried about.
 */

/** A JSON integer of whole dinars, not yet an Amount. */
export type RawAmount = number;

export type UUID = string;
/** ISO date, no time — a due date, a birth date. */
export type IsoDate = string;
/** RFC 3339 instant. */
export type IsoTime = string;

/* ------------------------------------------------------------------- auth */

export interface UserView {
  id: UUID;
  username: string;
  full_name: string;
  roles: Role[];
  must_change_password: boolean;
  scope_mode?: string;
}

export type Role =
  | "cashier"
  | "finance_manager"
  | "registrar"
  | "academic_officer"
  | "auditor"
  | "report_viewer"
  | "admin";

export interface TokenResponse {
  access_token: string;
  access_expires_at: IsoTime;
  refresh_token?: string;
  refresh_expires_at?: IsoTime;
  token_type: string;
  user: UserView;
}

/* ---------------------------------------------------------------- student */

export interface StudentView {
  id: UUID;
  student_no: string;
  full_name: string;
  /** Never a secondary field: two names identical after normalisation are told apart by it (§06). */
  mother_name: string;
  birth_date?: IsoDate | null;
  gender?: string | null;
  phone?: string | null;
  phone_alt?: string | null;
  email?: string | null;
  address?: string | null;
  guardian_name?: string | null;
  guardian_phone?: string | null;
  status: string;

  /**
   * Read-time convenience from the student's most recent non-superseded
   * enrollment — not a stored student attribute. Study type and stage still
   * live only on enrollment. Absent when the student has no enrollment.
   */
  current_study_type_id?: UUID | null;
  current_study_type_code?: string | null;
  current_stage?: number | null;
  current_academic_year_id?: UUID | null;
  /**
   * What the student owes and has paid, across every account of theirs that
   * was not cancelled. Read-time, like current_* above — absent when the
   * student has no account yet, which is not the same as having paid nothing.
   */
  effective_net?: RawAmount;
  paid_total?: RawAmount;
  outstanding?: RawAmount;
  /** 0..100, floored. A student owing nothing is 100. */
  paid_percent?: number;

  /** The day the office registered them, which is not when the row was made. */
  registered_on?: string;
}

/** The outcome of POST /students/intake — identity, placement, and pricing
 * in one request. */
export interface RegisterStudentWithPlacementView {
  student: StudentView;
  enrollment: EnrollmentView;
  /** Absent when pricing_pending is true. */
  account?: AccountView | null;
  /**
   * True when the enrollment was created but no account was generated with
   * it: either the actor lacks finance authority, or no fee policy matched
   * this enrollment's scope. pricing_note explains which.
   */
  pricing_pending: boolean;
  pricing_note?: string;
}

export interface EnrollmentView {
  id: UUID;
  student_id: UUID;
  academic_year_id: UUID;
  sequence_no: number;
  college_id: UUID;
  department_id: UUID;
  study_type_id: UUID;
  stage: number;
  attempt_number: number;
  kind: string;
  status: EnrollmentStatus;
  result: string;
  result_by_decision: boolean;
  supersedes_id?: UUID | null;
  supersede_reason?: string | null;
  is_repeat: boolean;
}

export type EnrollmentStatus =
  | "active"
  | "superseded"
  | "deferred"
  | "dropped"
  | "withdrawn"
  | "transferred"
  | "completed";

/* ---------------------------------------------------------------- account */

export interface AccountView {
  id: UUID;
  enrollment_id: UUID;
  student_id: UUID;
  academic_year_id: UUID;
  stage: number;
  status: AccountStatus;
  gross_total: RawAmount;
  discountable_base: RawAmount;
  discount_total: RawAmount;
  /** Set once at generation and never moved again. */
  net_snapshot: RawAmount;
  adjustment_total: RawAmount;
  /** net_snapshot + Σ adjustments — what is actually owed. */
  effective_net: RawAmount;
  paid_total: RawAmount;
  refunded_total: RawAmount;
  net_paid: RawAmount;
  credit_balance: RawAmount;
  remaining: RawAmount;
}

export type AccountStatus = "draft" | "active" | "settled" | "cancelled";

export interface SnapshotLineView {
  component_code: string;
  name_ar: string;
  amount: RawAmount;
  is_discountable: boolean;
  is_refundable: boolean;
}

export interface InstallmentView {
  id: UUID;
  number: number;
  due_date: IsoDate;
  amount: RawAmount;
  paid_amount: RawAmount;
  remaining: RawAmount;
  status: InstallmentStatus;
  /** Computed for the request, never read from a column — overdue is never stored. */
  is_overdue: boolean;
}

export type InstallmentStatus = "due" | "partially_paid" | "paid" | "superseded";

export interface PaymentView {
  id: UUID;
  /** Absent until the moment of posting: the number comes from a locked counter. */
  receipt_no?: string | null;
  account_id: UUID;
  student_id: UUID;
  amount: RawAmount;
  payment_method_id: UUID;
  method_reference?: string | null;
  status: PaymentStatus;
  paid_at: IsoTime;
  posted_at?: IsoTime | null;
  voided_at?: IsoTime | null;
  void_reason?: string | null;
  payer_name?: string | null;
}

export type PaymentStatus = "posted" | "voided";

export interface AllocationView {
  installment_id: UUID;
  amount: RawAmount;
}

export interface RecordPaymentResponse {
  payment: PaymentView;
  allocations: AllocationView[];
  credit_amount: RawAmount;
  remaining: RawAmount;
  /**
   * A replayed idempotency key. The terminal has already printed this receipt
   * once and must not print it again — §01 calls for showing "إعادة" instead.
   */
  duplicate: boolean;
}

export interface RefundView {
  id: UUID;
  refund_no?: string | null;
  payment_id: UUID;
  account_id: UUID;
  amount: RawAmount;
  reason: string;
  status: RefundStatus;
  requested_at: IsoTime;
  posted_at?: IsoTime | null;
}

export type RefundStatus = "requested" | "approved" | "posted" | "rejected";

export interface VoidRequestView {
  id: UUID;
  payment_id: UUID;
  reason: string;
  status: string;
  requested_at: IsoTime;
  executed_at?: IsoTime | null;
}

export interface DiscountApplicationView {
  id: UUID;
  assignment_id: UUID;
  frozen_base: RawAmount;
  /** What the rule computed. */
  computed_amount: RawAmount;
  /** What was actually applied. Both are kept so nothing is reduced silently. */
  applied_amount: RawAmount;
  truncation_reason?: string | null;
  status: string;
}

export interface AccountDetailView {
  account: AccountView;
  fee_components: SnapshotLineView[];
  discounts: DiscountApplicationView[];
  installments: InstallmentView[];
  payments: PaymentView[];
  refunds: RefundView[];
}

/* ------------------------------------------------------------------- year */

export interface YearView {
  id: UUID;
  code: string;
  start_date: IsoDate;
  end_date: IsoDate;
  status: YearStatus;
  debt_block_policy: "ignore" | "warn" | "block";
  /** Money may post. Cleared by the financial close. */
  accepts_financial_posting: boolean;
  /** Results may still be recorded. A year has two closes, and this is the second. */
  accepts_academic_recording: boolean;
  closed_at?: IsoTime | null;
}

export type YearStatus = "draft" | "open" | "financially_closed" | "closed" | "adjustment_open";

/* --------------------------------------------------------------- reference */

/**
 * Reference data as the shared routes return it.
 *
 * GET /colleges, /departments and /study-types are the trimmed views every
 * signed-in screen uses, and they carry **only** id, code, name and (for a
 * department) stage_count. They also filter to active rows server-side, so
 * `is_active` is absent by design rather than missing — a client that filters
 * on it removes every row, which is how an entire select ends up empty.
 *
 * The richer administrative views with is_active and name_en exist on the
 * config handlers, but no GET route exposes them.
 */
export interface CollegeView {
  id: UUID;
  code: string;
  name_ar: string;
  /** Absent on the reference route; present only if an admin view is added. */
  is_active?: boolean;
}

export interface DepartmentView {
  id: UUID;
  college_id: UUID;
  code: string;
  name_ar: string;
  /**
   * How many stages the programme has. Load-bearing: bulk promotion completes
   * an enrollment on a pass at the final stage rather than promoting into a
   * stage that does not exist and pricing it from a policy nobody wrote.
   */
  stage_count: number;
  is_active?: boolean;
}

export interface StudyTypeView {
  id: UUID;
  code: string;
  name_ar: string;
  is_active?: boolean;
}

export interface PaymentMethodView {
  id: UUID;
  code: string;
  name_ar: string;
  /** Cash reconciles the drawer; the rest do not (§09 close-of-shift). */
  is_cash?: boolean;
}

export interface CashierDeskView {
  id: UUID;
  code: string;
  name_ar: string;
  college_id?: UUID | null;
  is_active: boolean;
}

/* ------------------------------------------------------------------- desk */

export interface CashierSessionView {
  id: UUID;
  cashier_user_id: UUID;
  cashier_desk_id: UUID;
  academic_year_id: UUID;
  status: "open" | "closed" | "approved";
  opened_at: IsoTime;
  opening_float: RawAmount;
  closed_at?: IsoTime | null;
  expected_cash?: RawAmount | null;
  counted_cash?: RawAmount | null;
  variance?: RawAmount | null;
  variance_reason?: string | null;
  approved_by?: string | null;
  approved_at?: IsoTime | null;
}

/**
 * The shift sheet.
 *
 * Cash and non-cash sit in separate rows because only the cash rows reconcile
 * against the drawer — §09 names the failure of mixing them: a phantom
 * variance every evening.
 */
export interface CashierSessionSummaryView {
  session: CashierSessionView;
  opening_float: RawAmount;
  /** Computed from the transaction rows, never typed by the person being reconciled. */
  expected_cash: RawAmount;
  counted_cash?: RawAmount | null;
  variance?: RawAmount | null;
  variance_reason?: string | null;
  payments_by_method: SessionMethodView[];
  refunds: SessionTotalView;
  voids: SessionTotalView;
}

export interface SessionMethodView {
  method_code: string;
  is_cash: boolean;
  count: number;
  total: RawAmount;
  void_count: number;
  void_total: RawAmount;
}

export interface SessionTotalView {
  count: number;
  total: RawAmount;
}

/* ----------------------------------------------------------------- paging */

/** httpx.Page — the only envelope on the API, and only on list endpoints. */
export interface Page<T> {
  data: T[];
  total: number;
  limit: number;
  offset: number;
}

/* ----------------------------------------------------------------- config */

export interface FeeComponentView {
  id: string;
  code: string;
  name_ar: string;
  name_en?: string | null;
  amount: RawAmount;
  is_discountable: boolean;
  is_refundable: boolean;
  is_mandatory: boolean;
  sort_order: number;
}

export interface FeePolicyView {
  id: UUID;
  policy_code: string;
  version_no: number;
  academic_year_id: UUID;
  college_id?: UUID | null;
  department_id?: UUID | null;
  stage?: number | null;
  study_type_id?: UUID | null;
  student_category_id?: UUID | null;
  /** How narrowly the policy is scoped. Higher wins resolution. */
  specificity_score: number;
  status: "draft" | "published" | "retired";
  max_discount_bp: number;
  effective_from?: IsoDate | null;
  description?: string | null;
  components: FeeComponentView[];
  gross_total: RawAmount;
  published_at?: IsoTime | null;
  retired_at?: IsoTime | null;
}

export interface FeeResolutionCandidateView {
  policy_id: UUID;
  policy_code: string;
  version_no: number;
  specificity_score: number;
  /** Which of the six dimensions this policy pins down. */
  dimensions: string[];
  gross_total: RawAmount;
  wins: boolean;
}

export interface FeeResolutionPreviewView {
  resolvable: boolean;
  winner: FeeResolutionCandidateView | null;
  runners_up: FeeResolutionCandidateView[];
  explanation: string;
}

export interface TemplateLineView {
  line_no: number;
  share_bp: number;
  share_percent: string;
  /** Set when this line was authored as a literal amount rather than a percentage. */
  amount?: RawAmount | null;
  due_offset_days: number;
  label_ar?: string | null;
}

export interface InstallmentTemplateView {
  id: UUID;
  code: string;
  name_ar: string;
  name_en?: string | null;
  academic_year_id?: UUID | null;
  college_id?: UUID | null;
  department_id?: UUID | null;
  stage?: number | null;
  study_type_id?: UUID | null;
  specificity_score: number;
  max_installments: number;
  status: "draft" | "published" | "retired";
  lines: TemplateLineView[];
  /** Must reach 10000 basis points, or one line carries the remainder. */
  total_bp: number;
  published_at?: IsoTime | null;
  retired_at?: IsoTime | null;
}

export interface DiscountDefinitionView {
  id: UUID;
  code: string;
  name_ar: string;
  name_en?: string | null;
  category: string;
  exclusivity_group_id?: UUID | null;
  is_full_exemption: boolean;
  /** When true, each new year's application waits for eligibility to be confirmed. */
  annual_reconfirmation: boolean;
  is_active: boolean;
}

export interface DiscountVersionView {
  id: UUID;
  definition_id: UUID;
  version_no: number;
  value_type: "percentage" | "fixed";
  rate_bp?: number;
  rate_percent?: string | null;
  fixed_amount?: RawAmount;
  per_application_cap?: RawAmount | null;
  applies_to_components?: string[] | null;
  stackable: boolean;
  priority: number;
  requires_approval: boolean;
  approval_role?: string | null;
  required_documents?: string[] | null;
  valid_from_year_id?: UUID | null;
  valid_to_year_id?: UUID | null;
  status: "draft" | "published" | "retired";
  notes?: string | null;
  created_by?: string | null;
  published_at?: IsoTime | null;
  published_by?: string | null;
}

export interface DiscountDetailView {
  definition: DiscountDefinitionView;
  published_version: DiscountVersionView | null;
}

/* ------------------------------------------------------------------ bulk */

/** One previewed price. The hash is what a commit must present back. */
export interface AccountPlanRow {
  enrollment_id: UUID;
  student_id: UUID;
  stage: number;
  outcome: string;
  reason?: string;
  error_code?: string;
  /** Resolution's answer, shown and never chosen. */
  fee_policy_code?: string;
  fee_policy_specificity?: number;
  gross_total: RawAmount;
  discount_total: RawAmount;
  net_total: RawAmount;
  installments?: { number: number; due_date: IsoDate; amount: RawAmount }[];
  preview_hash?: string;
  account_id?: UUID | null;
}

export interface AccountBulkCounts {
  total: number;
  will_create: number;
  created: number;
  already_has_account: number;
  blocked: number;
  skipped: number;
  failed: number;
}

export interface GenerateAccountsBulkResult {
  dry_run: boolean;
  academic_year_id: UUID;
  outcome: string;
  counts: AccountBulkCounts;
  skip_reasons?: Record<string, number>;
  rows: AccountPlanRow[];
}

export interface PromotionRow {
  enrollment_id: UUID;
  student_id: UUID;
  student_no?: string;
  full_name?: string;
  from_stage?: number;
  to_stage?: number;
  outcome: string;
  reason?: string;
  error_code?: string;
}

export interface PromotionCounts {
  total: number;
  promoted: number;
  repeated: number;
  completed: number;
  skipped: number;
  failed: number;
}

export interface PromoteBulkResult {
  dry_run: boolean;
  source_year_id: UUID;
  target_year_id: UUID;
  outcome: string;
  counts: PromotionCounts;
  /** Fingerprints the whole plan; a commit must present it back. */
  plan_hash: string;
  skip_reasons?: Record<string, number>;
  rows: PromotionRow[];
}

/* ---------------------------------------------------------------- imports */

/**
 * The import batch and its rows.
 *
 * These come from internal/port and carry **no JSON tags**, so they serialise
 * with Go's field names in PascalCase. Declaring them any other way here
 * produces a screen of undefined that looks like an empty batch.
 */
export interface ImportBatchView {
  ID: UUID;
  BatchType: string;
  SourceFilename?: string | null;
  Status: ImportBatchStatus;
  AcademicYearID?: UUID | null;
  TotalRows: number;
  ValidRows: number;
  ErrorRows: number;
  CreatedRows: number;
  UpdatedRows: number;
  SkippedRows: number;
  FailedRows: number;
  /** Bumped as the worker progresses; a stopped heartbeat is a stalled batch. */
  HeartbeatAt?: IsoTime | null;
  StartedAt?: IsoTime | null;
  CompletedAt?: IsoTime | null;
  ErrorSummary?: string | null;
  CreatedAt: IsoTime;
}

export type ImportBatchStatus =
  | "uploaded"
  | "validating"
  | "validated"
  | "confirmed"
  | "importing"
  | "completed"
  | "failed"
  | "cancelled";

export interface ImportFinding {
  field?: string;
  code: string;
  message: string;
}

export interface ImportRowView {
  ID: UUID;
  BatchID: UUID;
  RowNo: number;
  RawData: Record<string, unknown>;
  ValidationStatus: string;
  Disposition: string;
  Errors?: ImportFinding[] | null;
  Warnings?: ImportFinding[] | null;
  MatchedEntityID?: UUID | null;
  CreatedEntityID?: UUID | null;
  ErrorMessage?: string | null;
}

export interface ImportRowCounts {
  Total: number;
  Pending: number;
  Valid: number;
  Warning: number;
  Error: number;
  Processed: number;
  Skipped: number;
  Failed: number;
  Created: number;
  Updated: number;
  /** Confirmation is gated on this reaching zero. */
  UnresolvedErrors: number;
}

export interface ImportReviewView {
  batch: ImportBatchView;
  counts: ImportRowCounts;
  rows: ImportRowView[];
  total: number;
}

/* ------------------------------------------------------------------ users */

export interface UserDetailView {
  id: UUID;
  username: string;
  full_name: string;
  email?: string | null;
  roles: Role[];
  is_active: boolean;
  must_change_password: boolean;
  scope_mode: string;
  colleges?: string[] | null;
  departments?: string[] | null;
  last_login_at?: IsoTime | null;
  locked_until?: IsoTime | null;
  disabled_reason?: string | null;
  created_at: IsoTime;
  cashier_desk_id?: UUID | null;
}

export interface SessionView {
  id: UUID;
  issued_at: IsoTime;
  expires_at: IsoTime;
  last_seen_at: IsoTime;
  revoked_at?: IsoTime | null;
  revoked_reason?: string | null;
  ip_address?: string | null;
  user_agent?: string | null;
  current: boolean;
}

export interface LoginAttemptView {
  username: string;
  succeeded: boolean;
  failure_code?: string | null;
  ip_address?: string | null;
  occurred_at: IsoTime;
}

export interface CreateUserResponse {
  user: UserDetailView;
  /** Present only when the server generated it, and only on this response. */
  temporary_password?: string;
}

/* ------------------------------------------------------------- enrollment */

/** What a status change did to the money. */
export interface TreatmentView {
  treatment: string;
  applied: boolean;
  waived: RawAmount;
  credit_raised: RawAmount;
  remaining_obligation: RawAmount;
  account_id?: UUID | null;
}

/** Graduation clearance — براءة الذمة. */
export interface ClearanceView {
  cleared: boolean;
  policy: string;
  outstanding: RawAmount;
  override_reason?: string | null;
  decided_at: IsoTime;
}

export interface ChangeStatusResponse {
  enrollment: EnrollmentView;
  financial_treatment?: TreatmentView | null;
  graduation_clearance?: ClearanceView | null;
}

/** The account generation result, for both the dry run and the commit. */
export interface GenerateAccountResult {
  account: AccountView;
  fee_components: SnapshotLineView[];
  discounts: DiscountApplicationView[];
  installments: InstallmentView[];
  pending_discounts?: unknown;
  dry_run: boolean;
}

/* --------------------------------------------------------------- sponsors */

export interface SponsorView {
  id: UUID;
  code: string;
  name_ar: string;
  name_en?: string | null;
  sponsor_type: string;
  contact_name?: string | null;
  contact_phone?: string | null;
  is_active: boolean;
}

export interface SponsorshipView {
  id: UUID;
  sponsor_id: UUID;
  student_id: UUID;
  coverage_type: "percentage" | "fixed_per_year" | "full";
  coverage_bp?: number | null;
  coverage_amount?: RawAmount | null;
  annual_cap?: RawAmount | null;
  /**
   * Decides who gets the debt letter: "receivable" leaves the student liable
   * and books the sponsor's share as expected inflow; "covers_debt" reduces
   * the student's debt and the university carries a sponsor default.
   */
  settlement_mode: "receivable" | "covers_debt";
  from_year_code: string;
  to_year_code?: string | null;
  status: string;
  agreement_ref?: string | null;
}

export interface SponsorReceivableView {
  sponsor_id: UUID;
  sponsor_code: string;
  sponsor_name: string;
  academic_year_id: UUID;
  commitment_count: number;
  student_count: number;
  committed: RawAmount;
  paid: RawAmount;
  outstanding: RawAmount;
}

/* ------------------------------------------------------------- settlement */

export interface SettlementBatchView {
  id: UUID;
  source_code: string;
  source_name?: string | null;
  filename: string;
  statement_from?: IsoDate | null;
  statement_to?: IsoDate | null;
  status: string;
  line_count: number;
  matched_count: number;
  total_amount: RawAmount;
  matched_amount: RawAmount;
}

export interface SettlementLineView {
  id: UUID;
  line_no: number;
  external_ref: string;
  amount: RawAmount;
  value_date?: IsoDate | null;
  description?: string;
  match_status: string;
  matched_payment_id?: UUID | null;
  /** Positive when the bank received more than the receipt says. */
  variance: RawAmount;
  review_note?: string | null;
}

export interface SettlementBatchDetailView {
  batch: SettlementBatchView;
  lines: SettlementLineView[];
}

export interface SettlementImportView {
  batch: SettlementBatchView;
  lines: SettlementLineView[];
  /** Lines a person still has to work. Zero is what clean looks like. */
  exceptions: number;
}

export interface SettlementExceptionView {
  line_id: UUID;
  batch_id: UUID;
  source_code: string;
  filename: string;
  line_no: number;
  external_ref?: string | null;
  amount: RawAmount;
  value_date?: IsoDate | null;
  match_status: string;
  variance: RawAmount;
  payment_id?: UUID | null;
}

export interface UnconfirmedPaymentView {
  payment_id: UUID;
  receipt_no?: string | null;
  student_id: UUID;
  amount: RawAmount;
  method_reference?: string | null;
  method_code: string;
  paid_at: IsoTime;
}

/* ---------------------------------------------------- electronic payments */

export interface PaymentProviderView {
  code: string;
  display_name: string;
}

export interface PaymentIntentView {
  id: UUID;
  provider: string;
  account_id: UUID;
  amount: RawAmount;
  status: string;
  provider_ref?: string | null;
  redirect_url?: string | null;
  /** What to tell the payer when there is no redirect: the counter reference. */
  instruction?: string;
  expires_at?: IsoTime | null;
  payment_id?: UUID | null;
  created_at: IsoTime;
}

/* --------------------------------------------- reconciliation, as operated */

export interface ReconciliationRunView {
  id: UUID;
  kind: string;
  status: string;
  started_at: IsoTime;
  finished_at?: IsoTime | null;
  rows_checked: number;
  findings: number;
  new_findings: number;
  error?: string | null;
}

export interface ReconciliationFindingView {
  id: UUID;
  kind: string;
  subject_type: string;
  subject_id: UUID;
  detail: Record<string, unknown>;
  severity: string;
  state: string;
  /** Passes that have seen it. Climbing on an untaken finding is the alarm. */
  seen_count: number;
  first_seen_at: IsoTime;
  last_seen_at: IsoTime;
  acknowledged_at?: IsoTime | null;
  acknowledged_reason?: string | null;
  resolved_at?: IsoTime | null;
  resolution?: string | null;
}

/* --------------------------------------------------- backup and restore */

export interface BackupView {
  id: UUID;
  kind: "manual" | "automatic" | "safety" | "imported";
  status: "running" | "verified" | "failed";
  started_at: IsoTime;
  finished_at?: IsoTime | null;
  bytes: number;
  verified: boolean;
  error?: string | null;
}

export interface RestoreCheckView {
  name: string;
  passed: boolean;
  detail?: string;
}

export interface RestoreView {
  id: UUID;
  backup_id: UUID;
  safety_backup_id?: UUID;
  status: "running" | "checking" | "swapping" | "restored" | "failed";
  started_at: IsoTime;
  finished_at?: IsoTime | null;
  checks?: RestoreCheckView[];
  error?: string | null;
}

export interface BackupScheduleView {
  enabled: boolean;
  interval_hours: number;
  retention_count: number;
  last_run_at?: IsoTime | null;
}

/* ---------------------------------------------------------------- hosting */

export interface HostingView {
  id: UUID;
  enrollment_id: UUID;
  direction: "incoming" | "outgoing";
  home_university?: string | null;
  home_college?: string | null;
  home_department?: string | null;
  host_university?: string | null;
  host_college?: string | null;
  host_department?: string | null;
  /** Which institution collects tuition — deliberately per-agreement. */
  fee_collector: string;
  generates_local_account: boolean;
  period_from?: IsoDate | null;
  period_to?: IsoDate | null;
  agreement_ref?: string | null;
}

/* --------------------------------------------------------- legal identity */

export interface IdentityVersionView {
  version_no: number;
  full_name: string;
  mother_name: string;
  effective_from?: IsoDate | null;
  court_decision_no?: string | null;
  court_decision_date?: IsoDate | null;
  reason: string;
  recorded_at: IsoTime;
}

export interface MergeStudentsResponse {
  source_student_id: UUID;
  target_student_id: UUID;
  enrollments_moved: number;
  accounts_moved: number;
  discounts_moved: number;
}

/* ------------------------------------------------------- installment plan */

export interface PlanRevisionView {
  id: UUID;
  kind: "reschedule" | "resplit";
  reason: string;
  plan_version: number;
  installments_before: number;
  installments_after: number;
  unpaid_before: RawAmount;
  unpaid_after: RawAmount;
  created_at: IsoTime;
}

export interface AdjustPlanResponse {
  installments: InstallmentView[];
  revision?: PlanRevisionView | null;
}

/* -------------------------------------------------------------- statement */

export interface FundingView {
  gross: RawAmount;
  discount: RawAmount;
  sponsor_covered: RawAmount;
  sponsor_receivable: RawAmount;
  sponsor_paid: RawAmount;
  student_paid: RawAmount;
  student_outstanding: RawAmount;
}

export interface StatementPaymentView {
  payment_id: UUID;
  receipt_no?: string | null;
  amount: RawAmount;
  method: string;
  paid_at: IsoTime;
  status: string;
  refunded: RawAmount;
}

export interface StatementAccountView {
  account_id: UUID;
  academic_year: string;
  gross: RawAmount;
  discount: RawAmount;
  effective_net: RawAmount;
  paid: RawAmount;
  outstanding: RawAmount;
  credit: RawAmount;
  status: string;
  installments: InstallmentView[];
  payments: StatementPaymentView[];
  funding?: FundingView | null;
}

export interface StudentStatementView {
  student_id: UUID;
  student_no: string;
  full_name: string;
  accounts: StatementAccountView[];
  total_charged: RawAmount;
  total_paid: RawAmount;
  outstanding: RawAmount;
  /** The soonest unpaid installment — the figure a student actually asks for. */
  next_due?: { due_date: IsoDate; amount: RawAmount; remaining: RawAmount } | null;
  generated_at: IsoTime;
}

export interface StatementVerificationView {
  code: string;
  expires_at: IsoTime;
  outstanding: RawAmount;
}

/* ---------------------------------------------------------- audit archive */

export interface ArchiveVerifyReport {
  ok: boolean;
  problems?: { kind: string; sequence_no?: number; artifact?: string; detail?: string }[];
}
