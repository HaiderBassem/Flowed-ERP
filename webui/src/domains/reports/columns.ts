/**
 * Column names, in Arabic.
 *
 * The reports are rendered generically — one table component for fifteen
 * reports, so that the money formatting and the count/money separation are
 * right in one place rather than fifteen. What generic rendering cannot do is
 * name a column, and `gross_total` in front of a dean is not a report.
 *
 * Anything not listed falls back to its raw key rather than a guess: an
 * unlabelled column the server grew should look unfamiliar, not look like
 * something else.
 */

export const COLUMN_LABELS: Record<string, string> = {
  // Identity and grouping.
  college_id: "معرّف الكلية",
  college_code: "رمز الكلية",
  college_name: "الكلية",
  department_id: "معرّف القسم",
  department_code: "رمز القسم",
  department_name: "القسم",
  study_type_code: "رمز الدراسة",
  study_type_name: "نوع الدراسة",
  stage: "المرحلة",
  academic_year_id: "السنة",
  year_code: "السنة",
  student_id: "معرّف الطالب",
  student_no: "الرقم الجامعي",
  full_name: "الاسم",
  mother_name: "اسم الأم",
  phone: "الهاتف",
  account_id: "الحساب",
  payment_id: "الدفعة",
  refund_id: "الاسترجاع",
  receipt_no: "رقم الوصل",
  refund_no: "رقم الاسترجاع",
  method_code: "طريقة الدفع",
  cashier_name: "الصراف",
  requested_by: "طلبه",
  executed_by: "نفّذه",
  approved_by: "وافق عليه",
  reason: "السبب",

  // Counts — deliberately never styled or grouped as money.
  student_count: "عدد الطلبة",
  account_count: "عدد الحسابات",
  enrollment_count: "عدد التسجيلات",
  installment_count: "عدد الأقساط",
  payment_count: "عدد الدفعات",
  count: "العدد",
  overdue_count: "عدد المتأخرة",
  void_count: "عدد الإلغاءات",
  refund_count: "عدد الاسترجاعات",

  // Money.
  gross_total: "إجمالي الرسوم",
  discount_total: "الخصومات",
  discountable_base: "الأساس القابل للخصم",
  net_snapshot: "الصافي المجمّد",
  adjustment_total: "قيود التسوية",
  effective_net: "الصافي الفعّال",
  paid_total: "المدفوع",
  net_paid: "المدفوع صافي",
  refunded_total: "المسترجَع",
  credit_balance: "رصيد دائن",
  remaining: "المتبقي",
  outstanding: "المتبقي",
  obligation: "الالتزام",
  collected: "المحصَّل",
  computed_amount: "المحتسب",
  applied_amount: "المطبَّق",
  amount: "المبلغ",
  expected_cash: "المتوقع نقداً",
  counted_cash: "المعدود",
  variance: "الفرق",
  opening_float: "الرصيد الافتتاحي",
  total: "المجموع",
  prior_years_collected: "تحصيل سنوات سابقة",

  // Dates and derived.
  due_date: "الاستحقاق",
  posted_at: "رُحِّل",
  paid_at: "قُبض",
  voided_at: "أُلغي",
  requested_at: "طُلب",
  day: "اليوم",
  month: "الشهر",
  bucket: "الشريحة",
  days_overdue: "أيام التأخر",
  collection_rate_pct: "نسبة التحصيل ٪",
  discount_rate_pct: "نسبة الخصم ٪",
  departments: "الأقسام",
  stages: "المراحل",
  buckets: "الشرائح",
  months: "الأشهر",
  gap_hours: "الفجوة (ساعات)",
  crossed_day: "عبر الأيام",
  is_overdue: "متأخر",
  status: "الحالة",
};

export const labelFor = (column: string): string => COLUMN_LABELS[column] ?? column;

/**
 * Columns worth hiding by default.
 *
 * A UUID column is noise on a printed report and pushes the figures off the
 * page; the identifier is still reachable by drilling into the row. It is
 * hidden rather than dropped from the export, which still carries everything.
 */
export const NOISY_COLUMNS = new Set([
  "college_id",
  "department_id",
  "academic_year_id",
  "student_id",
  "study_type_id",
]);
