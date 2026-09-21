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
  counter?: "voids" | "refunds";
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
    title: "اليومي",
    caption: "ما أعمله الآن",
    items: [
      { label: "قبض الأجور", to: "/desk", icon: "desk", capability: "payment.record" },
      { label: "الطلبة", to: "/students", icon: "students" },
      {
        label: "تسجيل طالب جديد",
        to: "/students/new",
        icon: "enroll",
        capability: "student.register",
      },
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
    ],
  },
  {
    title: "السنة الدراسية",
    caption: "الفتح والترقية",
    items: [
      { label: "السنوات الدراسية", to: "/years", icon: "years", capability: "year.administer" },
      {
        label: "تسجيل في سنة",
        to: "/enrollments/new",
        icon: "enroll",
        capability: "enrollment.write",
      },
      {
        label: "النجاح والرسوب والترقية",
        to: "/bulk/promotions",
        icon: "promote",
        capability: "enrollment.result",
      },
      {
        label: "توليد حسابات جماعي",
        to: "/bulk/accounts",
        icon: "bulk",
        capability: "account.generate",
      },
      { label: "الاستضافة", to: "/hosting", icon: "identity", capability: "hosting.write" },
    ],
  },
  {
    title: "الأجور والخصومات",
    caption: "ما يتجمّد في الحسابات",
    items: [
      {
        label: "أجور الدراسة والاستضافة",
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
        label: "أنواع الخصومات",
        to: "/config/discounts",
        icon: "discount",
        capability: "config.write",
      },
      {
        label: "الكليات والأقسام وأنواع الطلبة",
        to: "/config/reference",
        icon: "reference",
        capability: "config.write",
      },
    ],
  },
  {
    title: "التقارير والبيانات",
    caption: "الإخراج والإدخال",
    items: [
      { label: "التقارير", to: "/reports", icon: "reports", capability: "reports.read" },
      { label: "استيراد طلبة من ملف", to: "/imports", icon: "imports", capability: "import.run" },
      {
        label: "تصدير واستيراد بيانات النظام",
        to: "/admin/data",
        icon: "backup",
        capability: "data.transfer",
      },
      {
        label: "النسخ الاحتياطي",
        to: "/admin/backups",
        icon: "backup",
        capability: "backup.manage",
      },
    ],
  },
  {
    title: "السلامة",
    caption: "هل الحسابات سليمة",
    items: [
      {
        label: "المصالحة",
        to: "/oversight/reconciliation",
        icon: "scale",
        capability: "oversight.read",
      },
      {
        label: "سلسلة التدقيق",
        to: "/oversight/audit",
        icon: "shield",
        capability: "oversight.read",
      },
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
      {
        label: "الإعدادات",
        to: "/settings",
        icon: "reference",
        capability: "operators.administer",
      },
    ],
  },
];
