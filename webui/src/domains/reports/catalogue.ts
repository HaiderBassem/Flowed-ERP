/**
 * The fifteen reports — §09 and §11.
 *
 * `yearRequired` is not cosmetic. §07's FilterBar rule: the year is mandatory
 * on every financial report, and when it is empty the request is **not sent**
 * — a message explains that the year is what bounds the scan and that two
 * years' figures are not comparable. The cashier-daily report is the one
 * exception and it is exceptional for a reason worth stating: a cashier's day
 * belongs to a shift, not to an academic year, so it is bounded by dates.
 */

/**
 * A dimension the server actually filters on.
 *
 * Declared per report rather than offered everywhere, because a select that is
 * silently ignored is worse than an absent one: an officer narrows to one
 * department, gets the whole university back, and files it as the department's
 * figures. Each entry here matches a filter the handler reads.
 */
export type ReportDimension = "college" | "department" | "study_type" | "stage";

export interface ReportSpec {
  id: string;
  path: string;
  label: string;
  description: string;
  yearRequired: boolean;
  /** Offers a from/to bound. On cashier-daily it is the only bound there is. */
  dateRange?: boolean;
  /**
   * Both ends of the range are mandatory and the request is not sent without
   * them. Only cashier-daily: the server refuses it outright, because a
   * cashier's day belongs to a shift and the range is what bounds the scan.
   */
  datesRequired?: boolean;
  /** Which dimension selects this report's handler honours. */
  dimensions?: ReportDimension[];
  /** Counts and money come from different populations here; §11's rule bites. */
  mixesCountAndMoney?: boolean;
}

export const REPORTS: ReportSpec[] = [
  {
    id: "departments",
    path: "/reports/departments",
    label: "الأقسام",
    description: "الالتزام والتحصيل والدين لكل قسم.",
    yearRequired: true,
    dimensions: ["college", "department", "study_type", "stage"],
    mixesCountAndMoney: true,
  },
  {
    id: "study-types",
    path: "/reports/study-types",
    label: "أنواع الدراسة",
    description: "صباحي ومسائي وما بينهما.",
    yearRequired: true,
    dimensions: ["college", "department", "study_type", "stage"],
    mixesCountAndMoney: true,
  },
  {
    id: "stages",
    path: "/reports/stages",
    label: "المراحل",
    description: "التوزيع على المراحل الدراسية.",
    yearRequired: true,
    dimensions: ["college", "department", "study_type", "stage"],
    mixesCountAndMoney: true,
  },
  {
    id: "installments",
    path: "/reports/installments",
    label: "الأقساط",
    description: "الخطة مقابل ما حُصِّل منها.",
    yearRequired: true,
    dimensions: ["college", "department", "study_type"],
  },
  {
    id: "debt",
    path: "/reports/debt",
    label: "الديون",
    description: "من يدين وكم — قائمة تحصيل قابلة للتصدير.",
    yearRequired: true,
    dimensions: ["college", "department"],
  },
  {
    id: "aging",
    path: "/reports/aging",
    label: "الأعمار",
    description: "أعمار الدين بخمس شرائح؛ «سنوات سابقة» شريحة منفصلة لأنها حالة لا عمر.",
    yearRequired: true,
    dimensions: ["college", "department"],
  },
  {
    id: "discounts",
    path: "/reports/discounts",
    label: "الخصومات",
    description: "من صفوف التطبيق حصراً، بالمحتسب بجانب المطبَّق.",
    yearRequired: true,
  },
  {
    id: "exemptions",
    path: "/reports/exemptions",
    label: "الإعفاءات",
    description: "الإعفاءات الممنوحة وأساسها.",
    yearRequired: true,
    dateRange: true,
  },
  {
    id: "collection-trend",
    path: "/reports/collection-trend",
    label: "اتجاه التحصيل",
    description: "التحصيل التراكمي مقابل الالتزام عبر أشهر السنة.",
    yearRequired: true,
    dimensions: ["college", "department"],
  },
  {
    id: "cash-flow",
    path: "/reports/cash-flow",
    label: "التدفق المتوقع",
    description: "ما تعنيه تواريخ الاستحقاق المتبقية من تدفق.",
    yearRequired: true,
    dimensions: ["college", "department"],
  },
  {
    id: "cashier-daily",
    path: "/reports/cashier-daily",
    label: "الصندوق اليومي",
    description: "حركة النقد لكل صراف ويوم وطريقة.",
    yearRequired: false,
    dateRange: true,
    datesRequired: true,
  },
  {
    id: "voids",
    path: "/reports/voids",
    label: "الإلغاءات",
    description: "كل إلغاء، الأوسع فجوة أولاً.",
    yearRequired: true,
    dateRange: true,
  },
  {
    id: "refunds",
    path: "/reports/refunds",
    label: "الاسترجاعات",
    description: "كل استرجاع مُرحَّل بتوقيعيه.",
    yearRequired: true,
    dateRange: true,
  },
  {
    id: "year",
    path: "/reports/years",
    label: "السنة",
    description: "ملخص السنة — تحصيل السنوات السابقة سطر مستقل لا يُضاف إلى صافيها.",
    yearRequired: true,
  },
  {
    id: "statement",
    path: "/reports/students",
    label: "كشف الطالب",
    description: "يُفتح من ملف الطالب.",
    yearRequired: false,
  },
];
