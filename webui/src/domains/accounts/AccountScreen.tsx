import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal } from "@/api/errors";
import type { AccountDetailView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Waterfall } from "@/components/Waterfall";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { formatDateTime } from "@/lib/dates";
import { amount } from "@/lib/money";
import { AdjustSheet } from "./AdjustSheet";
import { InstallmentTable } from "./InstallmentTable";
import { PlanRevisions, PlanSheet } from "./PlanSheet";

type Tab = "installments" | "payments" | "refunds" | "discounts" | "adjustments";

const TABS: { id: Tab; label: string }[] = [
  { id: "installments", label: "الأقساط" },
  { id: "payments", label: "الدفعات" },
  { id: "refunds", label: "الاسترجاعات" },
  { id: "discounts", label: "الخصومات" },
  { id: "adjustments", label: "القيود" },
];

/**
 * The account page — §09.
 *
 * The waterfall on top, five tabs beneath it. The available commands are
 * derived from the account's status *and* the year's status together, and a
 * command that is not legitimate is disabled wearing its reason rather than
 * hidden.
 *
 * There is no edit and no delete. In their place sit the three legitimate
 * corrections, each of which writes a new visible document.
 */
export function AccountScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();
  const { years } = useWorkingContext();
  const [tab, setTab] = useState<Tab>(() =>
    (window.location.hash.slice(1) as Tab) || "installments",
  );
  const [adjusting, setAdjusting] = useState(false);
  const [planning, setPlanning] = useState(false);

  const account = useQuery({
    queryKey: ["account", id],
    queryFn: () => api.get<AccountDetailView>(`/accounts/${id}`),
    enabled: Boolean(id),
  });

  if (account.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="40%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  if (account.isError) {
    const error = account.error;
    return (
      <main className="screen">
        {isRefusal(error) ? (
          <RefusalPanel refusal={error} onRetry={() => void account.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح الحساب" />
        )}
      </main>
    );
  }

  const detail = account.data!;
  const view = detail.account;
  const year = years.find((y) => y.id === view.academic_year_id);

  // Legitimacy is the account state and the year state together. Either one
  // can be the thing that forbids the command, and the operator is told which.
  const yearFrozen = year ? !year.accepts_financial_posting : false;
  const adjustBlock = !can("account.adjust")
    ? reason("account.adjust")
    : view.status === "cancelled"
      ? "الحساب ملغى — لا يقبل قيوداً"
      : yearFrozen
        ? `السنة ${year?.code} مغلقة مالياً — القيود عليها مرفوضة`
        : undefined;

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: "ملف الطالب", to: `/students/${view.student_id}` },
          { label: `حساب ${year?.code ?? ""}` },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">الحساب المالي</h1>
        <StateChip entity="account" status={view.status} />
        {year && (
          <>
            <span className="num">{year.code}</span>
            <StateChip entity="year" status={year.status} />
          </>
        )}
        <span className="grow" />
        <Link className="btn" to={`/students/${view.student_id}`}>
          ملف الطالب
        </Link>
      </div>

      <div className="cols cols--half" style={{ marginTop: 14 }}>
        <Panel title="الشلال المالي" aside={<span className="label">لماذا هذا الرقم هو هذا الرقم</span>}>
          <Waterfall
            account={view}
            components={detail.fee_components}
            discounts={detail.discounts}
          />
        </Panel>

        <div className="stack">
          <Panel title="الأوامر المشروعة الآن">
            {/* Absent by design: there is no edit and no delete on anything
                financial. The three below are the corrections that exist. */}
            <p className="note" style={{ marginBottom: 10 }}>
              لا يوجد «تعديل» ولا «حذف» على أي وثيقة مالية. التصحيح يكون بواحد من هذه، وكل واحد
              يُنشئ وثيقة جديدة مرئية.
            </p>
            <div className="cluster">
              <Button
                disabled={Boolean(adjustBlock)}
                disabledReason={adjustBlock}
                onClick={() => setAdjusting(true)}
                title="قيد موقّع يغيّر الصافي الفعّال دون أن يمسّ الصافي المجمّد"
              >
                قيد تسوية
              </Button>
              <Button
                disabled={!can("plan.adjust") || view.status === "cancelled"}
                disabledReason={
                  !can("plan.adjust")
                    ? reason("plan.adjust")
                    : view.status === "cancelled"
                      ? "الحساب ملغى"
                      : undefined
                }
                onClick={() => setPlanning(true)}
                title="إعادة جدولة التواريخ أو إعادة تقسيم غير المدفوع — بمراجعة مسجّلة"
              >
                تعديل الخطة
              </Button>
              <Button
                disabled
                disabledReason="يُفتح من صفحة الوصل — الإلغاء يخص دفعة بعينها"
              >
                طلب إلغاء دفعة
              </Button>
              <Button
                disabled
                disabledReason="يُفتح من صفحة الوصل — الاسترجاع يخص دفعة بعينها"
              >
                طلب استرجاع
              </Button>
            </div>
          </Panel>

          {adjusting && <AdjustSheet account={view} onDone={() => setAdjusting(false)} />}
          {planning && (
            <PlanSheet
              accountId={view.id}
              installments={detail.installments}
              onDone={() => setPlanning(false)}
            />
          )}

          <Panel title="الأرقام كما هي">
            <Row label="الصافي المجمّد">
              <Money value={view.net_snapshot} tone="plain" />
            </Row>
            <Row label="مجموع القيود">
              <Money value={view.adjustment_total} sign="always" />
            </Row>
            <Row label="الصافي الفعّال">
              <Money value={view.effective_net} tone="positive" />
            </Row>
            <Row label="مدفوع إجمالاً">
              <Money value={view.paid_total} tone="plain" />
            </Row>
            <Row label="مسترجَع">
              <Money value={view.refunded_total} tone="plain" />
            </Row>
            <Row label="مدفوع صافي">
              <Money value={view.net_paid} tone="positive" />
            </Row>
            <Row label="رصيد دائن">
              <Money value={view.credit_balance} tone="plain" />
            </Row>
            <Row label="المتبقي">
              <Money value={view.remaining} size="big" />
            </Row>
          </Panel>
        </div>
      </div>

      <div style={{ marginTop: 14 }}>
        <Panel
          title={
            <div className="cluster">
              {TABS.map((t) => (
                <button
                  key={t.id}
                  type="button"
                  className={`sheet__stage`}
                  data-state={tab === t.id ? "current" : undefined}
                  style={{ border: 0, background: "transparent", cursor: "pointer" }}
                  onClick={() => {
                    setTab(t.id);
                    window.location.hash = t.id;
                  }}
                >
                  {t.label}
                  {counts(detail)[t.id] !== null && (
                    <span className="label"> ({counts(detail)[t.id]})</span>
                  )}
                </button>
              ))}
            </div>
          }
          flush
        >
          {tab === "installments" && (
            <>
              <InstallmentTable installments={detail.installments} />
              <PlanRevisions accountId={view.id} />
            </>
          )}

          {tab === "payments" && (
            <PaymentsTab detail={detail} />
          )}

          {tab === "refunds" && (
            detail.refunds.length === 0 ? (
              <EmptyState kind="not-yet" title="لا استرجاعات على هذا الحساب" />
            ) : (
              <table className="grid">
                <thead>
                  <tr>
                    <th>الرقم</th>
                    <th className="n">المبلغ</th>
                    <th>السبب</th>
                    <th>طُلب</th>
                    <th>الحالة</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.refunds.map((refund) => (
                    <tr key={refund.id}>
                      <td className="ltr num">{refund.refund_no ?? "—"}</td>
                      <td className="n">
                        <Money value={refund.amount} tone="plain" />
                      </td>
                      <td>{refund.reason}</td>
                      <td className="num">{formatDateTime(refund.requested_at)}</td>
                      <td>
                        <StateChip entity="refund" status={refund.status} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          )}

          {tab === "discounts" && (
            detail.discounts.length === 0 ? (
              <EmptyState kind="not-yet" title="لا خصومات مطبَّقة على هذا الحساب" />
            ) : (
              <table className="grid">
                <thead>
                  <tr>
                    <th>الأساس المجمّد</th>
                    <th className="n">المحتسب</th>
                    <th className="n">المطبَّق</th>
                    <th>سبب القصّ</th>
                    <th>الحالة</th>
                  </tr>
                </thead>
                <tbody>
                  {detail.discounts.map((d) => (
                    <tr key={d.id}>
                      <td className="n">
                        <Money value={d.frozen_base} tone="plain" />
                      </td>
                      {/* Both figures, always. §11: a truncated grant keeps
                          what it computed beside what it applied, so nothing
                          is reduced silently. */}
                      <td className="n">
                        <Money value={d.computed_amount} tone="plain" />
                      </td>
                      <td className="n">
                        <Money value={d.applied_amount} tone="plain" />
                      </td>
                      <td>{d.truncation_reason ?? "—"}</td>
                      <td>
                        <StateChip entity="discount_application" status={d.status} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )
          )}

          {tab === "adjustments" && (
            <div style={{ padding: 16 }}>
              <Row label="مجموع القيود">
                <Money value={view.adjustment_total} sign="always" />
              </Row>
              {/* Honest about a gap rather than rendering an empty table that
                  implies there are none. */}
              <p className="note" style={{ marginTop: 10 }}>
                يعيد الـ API مجموع القيود ولا يعيد صفوفها المفردة على هذا المسار، فلا يمكن سردها
                هنا. الأثر ظاهر في الشلال بين الصافي المجمّد والصافي الفعّال.
              </p>
            </div>
          )}
        </Panel>
      </div>
    </main>
  );
}

function PaymentsTab({ detail }: { detail: AccountDetailView }) {
  if (detail.payments.length === 0) {
    return <EmptyState kind="not-yet" title="لا دفعات على هذا الحساب بعد" />;
  }
  return (
    <table className="grid">
      <thead>
        <tr>
          <th>رقم الوصل</th>
          <th className="n">المبلغ</th>
          <th>رُحِّل</th>
          <th>الحالة</th>
        </tr>
      </thead>
      <tbody>
        {detail.payments.map((payment) => (
          // A voided receipt keeps its row and its number. Removing it removes
          // the evidence the void register is built to read.
          <tr key={payment.id} className={payment.status === "voided" ? "is-void" : undefined}>
            <td>
              <Link className="ltr num" to={`/payments/${payment.id}`}>
                {payment.receipt_no ?? "—"}
              </Link>
            </td>
            <td className="n">
              <Money value={payment.amount} tone="plain" />
            </td>
            <td className="num">{formatDateTime(payment.posted_at ?? payment.paid_at)}</td>
            <td>
              <StateChip entity="payment" status={payment.status} />
              {payment.void_reason && <span className="label"> — {payment.void_reason}</span>}
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function counts(detail: AccountDetailView): Record<Tab, number | null> {
  return {
    installments: detail.installments.length,
    payments: detail.payments.length,
    refunds: detail.refunds.length,
    discounts: detail.discounts.length,
    // The endpoint returns a total, not rows, so a count here would be a
    // guess. Null renders nothing rather than a misleading zero.
    adjustments: amount(detail.account.adjustment_total) === 0n ? 0 : null,
  };
}

export { Chip };
