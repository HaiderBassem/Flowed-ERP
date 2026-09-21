import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { RawAmount } from "@/api/types";
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
    queryFn: () => api.get<StatementReport>(`/reports/students/${id}/statement`),
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

  const view = flatten(statement.data!);

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

export { Chip };

/* ------------------------------------------------------------------ shape */

/**
 * The statement report, as the server sends it.
 *
 * Only the fields this screen reads are declared. The report carries a good
 * deal more — fee components, discounts, adjustments — and declaring all of it
 * here would be a second copy of the server's view struct to keep in step.
 */
interface StatementReport {
  student: { id: string; student_no: string; full_name: string };
  accounts: StatementAccount[];
  totals: { effective_net: RawAmount; net_paid: RawAmount; remaining: RawAmount };
}

interface StatementAccount {
  account_id: string;
  academic_year_code: string;
  account_status: string;
  gross_total: RawAmount;
  discount_total: RawAmount;
  effective_net: RawAmount;
  net_paid: RawAmount;
  credit_balance: RawAmount;
  remaining: RawAmount;
  installments: { due_date: string; remaining: RawAmount; status: string }[];
  payments: {
    payment_id: string;
    receipt_no: string | null;
    amount: RawAmount;
    method_code: string;
    status: string;
    paid_at: string;
  }[];
}

/**
 * flatten adapts the report to the shape this screen was written against.
 *
 * The portal computed a statement with its totals at the top level and called
 * the fields "total_charged", "total_paid" and "outstanding"; the report nests
 * them under "totals" and calls the last one "remaining". The screen is worth
 * more than the names, so the names are translated once, here, rather than at
 * forty call sites where the next renamed field becomes a silent zero.
 *
 * The next due date is derived rather than read: the report lists installments
 * and the statement wants the earliest one still owing, which is a question
 * about the list rather than a field on it.
 */
function flatten(report: StatementReport) {
  const due = report.accounts
    .flatMap((account) => account.installments)
    .filter((installment) => installment.status !== "paid" && amount(installment.remaining) > 0n)
    .sort((a, b) => a.due_date.localeCompare(b.due_date))[0];

  return {
    student_id: report.student.id,
    student_no: report.student.student_no,
    full_name: report.student.full_name,
    // The report is computed per request, so "now" is when it was computed.
    generated_at: new Date().toISOString(),
    total_charged: report.totals.effective_net,
    total_paid: report.totals.net_paid,
    outstanding: report.totals.remaining,
    next_due: due ? { due_date: due.due_date, remaining: due.remaining } : undefined,
    accounts: report.accounts.map((account) => ({
      account_id: account.account_id,
      academic_year: account.academic_year_code,
      status: account.account_status,
      gross: account.gross_total,
      discount: account.discount_total,
      effective_net: account.effective_net,
      paid: account.net_paid,
      credit: account.credit_balance,
      outstanding: account.remaining,
      payments: account.payments.map((payment) => ({
        payment_id: payment.payment_id,
        receipt_no: payment.receipt_no,
        amount: payment.amount,
        method: payment.method_code,
        status: payment.status,
        paid_at: payment.paid_at,
        // The report does not break a payment down by what was refunded from
        // it. Shown as nothing rather than as zero, because zero would assert
        // that none was returned.
        refunded: 0 as RawAmount,
      })),
    })),
  };
}
