/**
 * The state lexicon — §05, one badge per domain state, defined once.
 *
 * Every status shown anywhere in the interface resolves through this table.
 * The point is not tidiness: a status that gets its colour at the call site
 * eventually gets a different colour at a different call site, and the moment
 * green means two things the lexicon has stopped being readable.
 *
 * The tones are the semantic colours and nothing else:
 *   live    — money that is alive and settled. Never "operation succeeded".
 *   pending — waiting on a second signature.
 *   void    — voided, refused, destructive.
 *   frozen  — immutable, snapshotted, superseded.
 *   muted   — neutral, or finished and no longer interesting.
 */

export type Tone = "live" | "pending" | "void" | "frozen" | "muted";

export interface StateLabel {
  label: string;
  tone: Tone;
  /** Shown on hover — several of these states are not self-explanatory. */
  hint?: string;
}

type Lexicon = Record<string, StateLabel>;

const YEAR: Lexicon = {
  draft: { label: "مسودة", tone: "muted" },
  open: { label: "مفتوحة", tone: "live" },
  financially_closed: {
    label: "مغلقة مالياً",
    tone: "frozen",
    hint: "المال مجمّد، والتسجيل الأكاديمي ما زال مفتوحاً — نتائج الدور الثاني تصل بعد إغلاق الخزينة",
  },
  closed: { label: "مغلقة", tone: "muted" },
  adjustment_open: {
    label: "معاد فتحها للتسوية",
    tone: "void",
    hint: "فتح استثنائي موقوت لتصحيح مدقَّق، حتى يُغلق مجدداً",
  },
};

const ENROLLMENT: Lexicon = {
  active: { label: "فعّال", tone: "live" },
  superseded: {
    label: "مستبدَل",
    tone: "frozen",
    hint: "استُبدل بتسجيل آخر؛ حسابه أُلغي ورصيده نُقل بقيدين ظاهرين",
  },
  deferred: { label: "مؤجَّل", tone: "pending" },
  dropped: { label: "منقطع", tone: "muted" },
  withdrawn: { label: "منسحب", tone: "muted" },
  transferred: { label: "منقول", tone: "muted" },
  completed: { label: "مكتمل", tone: "live" },
};

const ACCOUNT: Lexicon = {
  draft: { label: "قيد الإعداد", tone: "pending" },
  active: { label: "فعّال", tone: "live" },
  settled: { label: "مسدَّد", tone: "live" },
  cancelled: {
    label: "ملغى",
    tone: "void",
    hint: "يبقى ظاهراً: غالباً هو سبب وجود الحساب الذي خلفه",
  },
};

const INSTALLMENT: Lexicon = {
  due: { label: "مستحق", tone: "muted" },
  partially_paid: { label: "مدفوع جزئياً", tone: "pending" },
  paid: { label: "مدفوع", tone: "live" },
  superseded: { label: "مستبدَل", tone: "frozen" },
};

const PAYMENT: Lexicon = {
  posted: { label: "مُرحَّلة", tone: "live" },
  voided: { label: "ملغاة", tone: "void" },
};

const REFUND: Lexicon = {
  requested: { label: "مطلوب", tone: "pending" },
  approved: { label: "موافَق", tone: "pending" },
  posted: { label: "مُرحَّل", tone: "live" },
  rejected: { label: "مرفوض", tone: "void" },
};

const DISCOUNT_APPLICATION: Lexicon = {
  pending_confirmation: {
    label: "ينتظر تأكيد الأهلية",
    tone: "pending",
    hint: "الرسم كامل حتى التأكيد — الخصم ليس مطبَّقاً بعد",
  },
  awaiting_confirmation: { label: "ينتظر تأكيد الأهلية", tone: "pending" },
  applied: { label: "مطبَّق", tone: "live" },
  reversed: { label: "معكوس", tone: "void" },
  rejected: { label: "مرفوض", tone: "muted" },
};

const SHIFT: Lexicon = {
  open: { label: "مفتوحة", tone: "live" },
  closed: {
    label: "مغلقة تنتظر اعتماداً",
    tone: "pending",
    hint: "الصراف لا يعتمد درجه — الاعتماد صلاحية المدير المالي",
  },
  approved: { label: "معتمدة", tone: "frozen" },
};

const VOID_REQUEST: Lexicon = {
  pending: { label: "ينتظر تنفيذاً", tone: "pending" },
  executed: { label: "نُفِّذ", tone: "void" },
  rejected: { label: "مرفوض", tone: "muted" },
};

export type Entity =
  | "year"
  | "enrollment"
  | "account"
  | "installment"
  | "payment"
  | "refund"
  | "discount_application"
  | "shift"
  | "void_request";

const TABLES: Record<Entity, Lexicon> = {
  year: YEAR,
  enrollment: ENROLLMENT,
  account: ACCOUNT,
  installment: INSTALLMENT,
  payment: PAYMENT,
  refund: REFUND,
  discount_application: DISCOUNT_APPLICATION,
  shift: SHIFT,
  void_request: VOID_REQUEST,
};

/**
 * describe resolves a raw status to its label and tone.
 *
 * An unknown status is shown as itself in the neutral tone rather than hidden
 * or coerced into a familiar one. A state the server grew and the interface
 * has not learned should look unfamiliar, not look like something else.
 */
export function describe(entity: Entity, status: string | null | undefined): StateLabel {
  if (!status) return { label: "—", tone: "muted" };
  const found = TABLES[entity][status];
  if (found) return found;
  return { label: status, tone: "muted", hint: "حالة غير معروفة لهذه الواجهة" };
}

/**
 * Overdue is derived at display time and is not in any table above, because it
 * is not a stored state: it is `due_date < today` with money outstanding.
 */
export function overdueLabel(days: number): StateLabel {
  return {
    label: `متأخر ${days} يوم`,
    tone: "void",
    hint: "محسوب من التاريخ لحظة العرض — لا حالة مخزّنة",
  };
}

/* ---------------------------------------------------------------------------
   Business labels for raw domain enums.

   Nothing below is a *status* in the chip sense — these are descriptive
   values (a kind, a policy, a result) that were reaching the screen as their
   wire form: "regular", "warn", "passed_r1". A cashier or registrar reads
   business Arabic, not enum names, so every raw value resolves here and an
   unknown one falls back to itself rather than being hidden.
   --------------------------------------------------------------------------- */

const STUDENT_STATUS: Record<string, string> = {
  active: "فعّال",
  inactive: "غير فعّال",
  suspended: "موقوف",
  merged: "مدموج في سجل آخر",
  archived: "مؤرشف",
};

const ENROLLMENT_KIND: Record<string, string> = {
  regular: "اعتيادي",
  hosted_in: "مستضاف من جامعة أخرى",
  transfer_in: "منتقل إلينا",
};

const EXAM_RESULT: Record<string, string> = {
  pending: "لم تُسجَّل بعد",
  passed_r1: "ناجح — دور أول",
  passed_r2: "ناجح — دور ثانٍ",
  failed: "راسب",
};

const DEBT_POLICY: Record<string, string> = {
  ignore: "يتجاهل الدين السابق",
  warn: "يحذّر ولا يحجب",
  block: "يحجب التسجيل حتى التسديد",
};

const IMPORT_DISPOSITION: Record<string, string> = {
  create: "سيُنشأ",
  update: "سيُحدَّث",
  skip: "تخطٍّ",
  error: "خطأ",
};

const DISCOUNT_CATEGORY: Record<string, string> = {
  social: "اجتماعي",
  staff: "أبناء التدريسيين والموظفين",
  merit: "تفوّق",
  exemption: "إعفاء",
  sibling: "أشقاء",
  martyr: "ذوو الشهداء",
  other: "أخرى",
};

// Study type is master data (§ "whatever the ministry introduces next" in
// academic.StudyType) — MORNING/EVENING/PARALLEL are just the seeded codes,
// so an unrecognised one falls back to itself exactly like every other table
// here, and to the muted chip tone rather than a made-up colour.
const STUDY_TYPE: Record<string, string> = {
  MORNING: "صباحي",
  EVENING: "مسائي",
  PARALLEL: "موازي",
};

const STUDY_TYPE_TONE: Record<string, string> = {
  MORNING: "study-morning",
  EVENING: "study-evening",
  PARALLEL: "study-parallel",
};

const label = (table: Record<string, string>) => (value: string | null | undefined) =>
  value ? (table[value] ?? value) : "—";

export const labelStudentStatus = label(STUDENT_STATUS);
export const labelEnrollmentKind = label(ENROLLMENT_KIND);
export const labelResult = label(EXAM_RESULT);
export const labelDebtPolicy = label(DEBT_POLICY);
export const labelDisposition = label(IMPORT_DISPOSITION);
export const labelDiscountCategory = label(DISCOUNT_CATEGORY);
export const labelStudyType = label(STUDY_TYPE);

/** Chip tone suffix ("chip--{suffix}") for a study type code, muted if unknown. */
export function studyTypeChipTone(code: string | null | undefined): string {
  if (!code) return "muted";
  return STUDY_TYPE_TONE[code] ?? "muted";
}
