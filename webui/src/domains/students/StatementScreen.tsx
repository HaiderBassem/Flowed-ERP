import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { StatementVerificationView, StudentStatementView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDate, formatDateTime } from "@/lib/dates";
import { amount } from "@/lib/money";

/**
 * The student statement — كشف الطالب — as the portal computes it.
 *
 * One page per year, and the figure a student actually came to ask about —
 * the soonest unpaid installment — pinned at the top rather than buried in a
 * table. The funding block appears when a sponsor is involved, because "who
 * pays" is the statement's second question.
 *
 * The verification code turns a printed page into a checkable document: a
 * third party enters the code at /verify/statement/<code> with no account and
 * learns only that this university issued these figures for this person —
 * nothing more. Codes expire, because a two-year-old clearance presented as
 * current is exactly the fraud the expiry prevents.
 */
export function StatementScreen() {
  const { id } = useParams<{ id: string }>();
  const { can } = useSession();

  const statement = useQuery({
    queryKey: ["statement", id],
    queryFn: () => api.get<StudentStatementView>(`/portal/students/${id}/statement`),
    enabled: Boolean(id),
  });

  if (statement.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="40%" />
        <div style={{ height: 12 }} />
        <Skeleton height={280} />
      </main>
    );
  }

  if (statement.isError) {
    return (
      <main className="screen">
        {isRefusal(statement.error) ? (
          <RefusalPanel refusal={statement.error} onRetry={() => void statement.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر تحضير الكشف" />
        )}
      </main>
    );
  }

  const view = statement.data!;

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: view.full_name, to: `/students/${view.student_id}` },
          { label: "كشف الطالب" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">كشف الطالب — {view.full_name}</h1>
        <span className="num">{view.student_no}</span>
        <span className="grow" />
        <Link className="btn no-print" to={`/students/${view.student_id}`}>
          ملف الطالب
        </Link>
        {can("statement.export") && <StatementExport studentId={view.student_id} />}
        <Button onClick={() => window.print()}>طباعة</Button>
      </div>

      {/* The context header every printed report carries (§11). */}
      <dl className="report-context">
        <div>
          <dt>الطالب:</dt>
          <dd>{view.full_name}</dd>
        </div>
        <div>
          <dt>الرقم الجامعي:</dt>
          <dd className="num">{view.student_no}</dd>
        </div>
        <div>
          <dt>لحظة التوليد:</dt>
          <dd className="num">{formatDateTime(view.generated_at)}</dd>
        </div>
      </dl>

      <div className="deck" style={{ marginBottom: 12 }}>
        <Stat value={<Money value={view.total_charged} tone="plain" />} label="إجمالي ما فُرض" />
        <Stat value={<Money value={view.total_paid} tone="plain" />} label="إجمالي ما دُفع" tone="live" />
        <Stat
          value={<Money value={view.outstanding} tone="plain" />}
          label="المتبقي — تجميع قراءة عبر السنوات"
          tone={amount(view.outstanding) > 0n ? "void" : "live"}
        />
        {view.next_due && (
          <Stat
            value={<Money value={view.next_due.remaining} tone="plain" />}
            label={`أقرب استحقاق — ${formatDate(view.next_due.due_date)}`}
            tone="pending"
          />
        )}
      </div>

      {(view.accounts ?? []).map((account) => (
        <Panel
          key={account.account_id}
          title={
            <>
              <span className="num">{account.academic_year}</span>{" "}
              <StateChip entity="account" status={account.status} />
            </>
          }
          aside={
            <Link className="label no-print" to={`/accounts/${account.account_id}`}>
              فتح الحساب ←
            </Link>
          }
        >
          <div className="cols cols--half">
            <div>
              <Row label="الإجمالي">
                <Money value={account.gross} tone="plain" />
              </Row>
              <Row label="الخصومات">
                <Money value={-amount(account.discount)} />
              </Row>
              <Row label="الصافي الفعّال">
                <Money value={account.effective_net} tone="plain" />
              </Row>
              <Row label="المدفوع">
                <Money value={account.paid} tone="positive" />
              </Row>
              <Row label="المتبقي">
                <Money value={account.outstanding} />
              </Row>
              {amount(account.credit) > 0n && (
                <Row label="رصيد دائن">
                  <Money value={account.credit} tone="positive" />
                </Row>
              )}
            </div>

            {/* Who is paying — the funding split, when a sponsor exists. */}
            {account.funding && (
              <div>
                <span className="field__label">من يدفع</span>
                <Row label="يغطيه كفيل">
                  <Money value={account.funding.sponsor_covered} tone="plain" />
                </Row>
                <Row label="ذمة على الكفيل">
                  <Money value={account.funding.sponsor_receivable} tone="plain" />
                </Row>
                <Row label="دفعه الكفيل">
                  <Money value={account.funding.sponsor_paid} tone="positive" />
                </Row>
                <Row label="دفعه الطالب">
                  <Money value={account.funding.student_paid} tone="positive" />
                </Row>
                <Row label="على الطالب">
                  <Money value={account.funding.student_outstanding} />
                </Row>
              </div>
            )}
          </div>

          {(account.payments ?? []).length > 0 && (
            <table className="grid" style={{ marginTop: 10 }}>
              <thead>
                <tr>
                  <th>الوصل</th>
                  <th>الطريقة</th>
                  <th className="n col-group-money">المبلغ</th>
                  <th className="n">مسترجَع منه</th>
                  <th>التاريخ</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {(account.payments ?? []).map((payment) => (
                  <tr
                    key={payment.payment_id}
                    className={payment.status === "voided" ? "is-void" : undefined}
                  >
                    <td>
                      <Link className="k ltr no-print" to={`/payments/${payment.payment_id}`}>
                        {payment.receipt_no ?? "—"}
                      </Link>
                      <span className="k ltr print-only">{payment.receipt_no ?? "—"}</span>
                    </td>
                    <td>{payment.method}</td>
                    <td className="n col-group-money">
                      <Money value={payment.amount} tone="plain" />
                    </td>
                    <td className="n">
                      {amount(payment.refunded) > 0n ? (
                        <Money value={-amount(payment.refunded)} />
                      ) : (
                        <span className="label">—</span>
                      )}
                    </td>
                    <td className="num">{formatDateTime(payment.paid_at)}</td>
                    <td>
                      <StateChip entity="payment" status={payment.status} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      ))}

      <VerificationPanel studentId={view.student_id} />
    </main>
  );
}

/**
 * The statement as a file — CSV for a spreadsheet, Excel for a clerk who has
 * to sort and total.
 *
 * It is the ledger rather than the summary on this page: every frozen fee
 * component, every discount with the reason it was cut short, every
 * adjustment, the schedule, and every payment including the voided ones. A
 * student disputing a balance is usually holding a receipt that was reversed,
 * and a file that omitted it could not explain the disagreement.
 *
 * The rendered page is what طباعة prints, so there is no third button for the
 * server's print-ready page: two ways to print one statement is two documents
 * that can disagree.
 */
function StatementExport({ studentId }: { studentId: string }) {
  const [running, setRunning] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const run = async (format: "csv" | "xlsx") => {
    setRunning(format);
    setRefusal(null);
    try {
      await api.download(`/reports/students/${studentId}/statement`, { format });
    } catch (error) {
      if (isRefusal(error)) setRefusal(error);
      else throw error;
    } finally {
      setRunning(null);
    }
  };

  return (
    <>
      <Button variant="ghost" disabled={running !== null} onClick={() => void run("csv")}>
        {running === "csv" ? "…CSV" : "CSV"}
      </Button>
      <Button variant="ghost" disabled={running !== null} onClick={() => void run("xlsx")}>
        {running === "xlsx" ? "…Excel" : "Excel"}
      </Button>
      {refusal && (
        <div className="no-print" style={{ flexBasis: "100%" }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}
    </>
  );
}

function VerificationPanel({ studentId }: { studentId: string }) {
  const [days, setDays] = useState("30");
  const [issued, setIssued] = useState<StatementVerificationView | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const issue = useMutation({
    mutationFn: () =>
      api.post<StatementVerificationView>(`/portal/students/${studentId}/statement/verification`, {
        valid_for_days: Number(days) || 30,
      }),
    onSuccess: (result) => {
      setIssued(result);
      setRefusal(null);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <div style={{ marginTop: 12 }}>
      <Panel
        title="رمز تحقق للنسخة المطبوعة"
        aside={<span className="label">يجعل الورقة قابلة للفحص من طرف ثالث</span>}
      >
        <p className="note" style={{ marginBottom: 10 }}>
          دائرة تستلم الكشف تدخل الرمز على{" "}
          <span className="k ltr">/verify/statement/&lt;code&gt;</span> بلا حساب، فتعلم أن هذه
          الجامعة أصدرت هذه الأرقام لهذا الشخص — ولا شيء غير ذلك. الرمز ينتهي، لأن براءة ذمة
          عمرها سنتان تُقدَّم على أنها حالية هي بالضبط ما تمنعه الصلاحية.
        </p>

        <div className="cluster no-print">
          <label className="field" style={{ margin: 0, maxWidth: 160 }}>
            <span className="field__label">صالح لأيام</span>
            <input
              className="input ltr"
              inputMode="numeric"
              value={days}
              onChange={(e) => setDays(e.target.value)}
            />
          </label>
          <Button variant="primary" busy={issue.isPending} onClick={() => issue.mutate()}>
            إصدار رمز
          </Button>
        </div>

        {refusal && (
          <div style={{ marginTop: 10 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}

        {issued && (
          <div className="callout" style={{ marginTop: 10 }}>
            <Row label="الرمز">
              <b className="ltr num" style={{ fontSize: "1.2em" }}>
                {issued.code}
              </b>
            </Row>
            <Row label="ينتهي">
              <span className="num">{formatDateTime(issued.expires_at)}</span>
            </Row>
            <Row label="المتبقي المُوثَّق">
              <Money value={issued.outstanding} tone="plain" />
            </Row>
            <p className="note" style={{ marginTop: 6 }}>
              اطبع الكشف بعد الإصدار ليخرج الرمز على الورقة.
            </p>
          </div>
        )}
      </Panel>
    </div>
  );
}

export { Chip };
