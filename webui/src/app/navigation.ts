import type { Capability } from "./session";

/**
 * The navigation map — §04, built from what the operator can actually do.
 *
 * Three questions, three planes — what must I do now, where is this object's
 * record, is the system sound — plus the two operational groups (bulk work and
 * financial configuration) that sit between records and governance the way an
 * ERP module tree does.
 *
 * Items are filtered by capability rather than hidden with CSS, and the
 * counters on the inbox items are live because a pending document is waiting
 * on a *person*: a void request that sleeps a week leaves a student holding a
 * receipt for an unresolved transaction.
 */

export interface NavItem {
  label: string;
  to: string;
  icon: string;
  capability?: Capability;
  /** Which live counter, if any, belongs beside this item. */
  counter?: "voids" | "refunds" | "settlement_exceptions";
  /**
   * The specification asks for this screen but the API exposes no way to build
   * it. Shown, and honest about why, rather than quietly dropped — a missing
   * item is indistinguishable from a permission the operator lacks.
   */
  unavailable?: string;
}

export interface NavPlane {
  title: string;
  caption: string;
  items: NavItem[];
}

export const PLANES: NavPlane[] = [
  {
    title: "العمل",
    caption: "ما يخصّني الآن",
    items: [
      { label: "شبّاك القبض", to: "/desk", icon: "desk", capability: "payment.record" },
      { label: "وردياتي", to: "/desk/sessions", icon: "shift", capability: "shift.open" },
      {
        label: "طلبات الإلغاء",
        to: "/inbox/voids",
        icon: "void",
        capability: "payment.void.execute",
        counter: "voids",
      },
      {
        label: "الاسترجاعات",
        to: "/inbox/refunds",
        icon: "refund",
        capability: "refund.approve",
        counter: "refunds",
      },
      {
        label: "استثناءات التسوية",
        to: "/settlements?tab=exceptions",
        icon: "bank",
        capability: "settlement.write",
        counter: "settlement_exceptions",
      },
      {
        label: "خصومات تنتظر تأكيداً",
        to: "/inbox/discounts",
        icon: "discount",
        capability: "discount.confirm",
        unavailable: "لا يوفّر الـ API قائمة بالتطبيقات المعلّقة",
      },
      {
        label: "ورديات تنتظر اعتماداً",
        to: "/inbox/shifts",
        icon: "approve",
        capability: "shift.approve",
        unavailable: "لا يوفّر الـ API قائمة بالورديات المغلقة",
      },
    ],
  },
  {
    title: "السجلات",
    caption: "أين الكائن",
    items: [
      { label: "الطلبة", to: "/students", icon: "students" },
      {
        label: "تسجيل في سنة",
        to: "/enrollments/new",
        icon: "enroll",
        capability: "enrollment.write",
      },
      { label: "السنوات الدراسية", to: "/years", icon: "years" },
      { label: "الكفلاء", to: "/sponsors", icon: "sponsors", capability: "settlement.read" },
      { label: "الاستضافة", to: "/hosting", icon: "identity", capability: "hosting.write" },
      { label: "الاستيراد", to: "/imports", icon: "imports", capability: "import.run" },
    ],
  },
  {
    title: "الجماعي",
    caption: "بمعاينة إلزامية",
    items: [
      {
        label: "توليد حسابات جماعي",
        to: "/bulk/accounts",
        icon: "bulk",
        capability: "account.generate",
      },
      {
        label: "الترقية الجماعية",
        to: "/bulk/promotions",
        icon: "promote",
        capability: "enrollment.result",
      },
    ],
  },
  {
    title: "الإعدادات المالية",
    caption: "ما يتجمّد في الحسابات",
    items: [
      {
        label: "سياسات الرسوم",
        to: "/config/fee-policies",
        icon: "policy",
        capability: "config.write",
      },
      {
        label: "قوالب الأقساط",
        to: "/config/installment-templates",
        icon: "template",
        capability: "config.write",
      },
      {
        label: "تعريفات الخصومات",
        to: "/config/discounts",
        icon: "discount",
        capability: "config.write",
      },
      {
        label: "البيانات المرجعية",
        to: "/config/reference",
        icon: "reference",
        capability: "operators.administer",
      },
    ],
  },
  {
    title: "الحوكمة",
    caption: "هل النظام سليم",
    items: [
      { label: "التقارير", to: "/reports", icon: "reports", capability: "reports.read" },
      {
        label: "التسويات البنكية",
        to: "/settlements",
        icon: "bank",
        capability: "settlement.read",
      },
      {
        label: "المصالحة",
        to: "/oversight/reconciliation",
        icon: "scale",
        capability: "oversight.read",
      },
      { label: "سلسلة التدقيق", to: "/oversight/audit", icon: "shield", capability: "oversight.read" },
      {
        label: "سجل الإلغاءات",
        to: "/oversight/voids",
        icon: "register",
        capability: "oversight.read",
      },
      {
        label: "المستخدمون والجلسات",
        to: "/admin/operators",
        icon: "operators",
        capability: "operators.administer",
      },
    ],
  },
];
