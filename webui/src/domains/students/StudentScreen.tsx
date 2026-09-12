import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal } from "@/api/errors";
import type { AccountView, EnrollmentView, StudentView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Waterfall } from "@/components/Waterfall";
import { EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { formatDateTime } from "@/lib/dates";
import { amount, format, sum, type Amount } from "@/lib/money";
import { formatPhone } from "@/lib/text";
import { labelEnrollmentKind, labelResult, labelStudentStatus } from "@/design/lexicon";
import { SponsorshipPanel } from "@/domains/sponsors/SponsorshipPanel";

/**
 * The student file — §09.
 *
 * Four sections in one page: identity, the academic chain with its supersede
 * lineage visible, the accounts one card per year, and the timeline.
 *
 * The rule that shapes it: **there is no single number that is "the student's
 * balance"**. The financial unit is the enrollment, not the student. A total
 * is shown, but it is labelled as a read-time aggregation across N accounts
 * and never as though it were one account's balance — otherwise "how much does
 * he owe for 2024-2025?" has no answer, and the fact that some of the debt
 * sits on a closed year's account is hidden.
 *
 * A cancelled account is displayed, not hidden: it is usually the reason the
 * account that replaced it exists, and a statement showing one without the
 * other cannot explain the pair.
 */
export function StudentScreen() {
  const { id } = useParams<{ id: string }>();
  const { can } = useSession();
  const { years } = useWorkingContext();

  const student = useQuery({
    queryKey: ["student", id],
    queryFn: () => api.get<StudentView>(`/students/${id}`),
    enabled: Boolean(id),
  });

  const enrollments = useQuery({
    queryKey: ["student-enrollments", id],
    queryFn: () => api.get<EnrollmentView[]>(`/students/${id}/enrollments`),
    enabled: Boolean(id),
  });

  const accounts = useQuery({
    queryKey: ["student-accounts", id],
    queryFn: () => api.get<AccountView[]>(`/students/${id}/accounts`),
    enabled: Boolean(id),
  });

  const audit = useQuery({
    queryKey: ["student-audit", id],
    queryFn: () => api.get<AuditRow[]>(`/students/${id}/audit`),
    enabled: Boolean(id) && can("oversight.read"),
    retry: false,
  });

  if (student.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={30} width="45%" />
        <div style={{ height: 14 }} />
        <Skeleton height={260} />
      </main>
    );
  }

  if (student.isError) {
    return (
      <main className="screen">
        {isRefusal(student.error) ? (
          <RefusalPanel refusal={student.error} onRetry={() => void student.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح ملف الطالب" />
        )}
      </main>
    );
  }

  const view = student.data!;
  const accountRows = accounts.data ?? [];
  const live = accountRows.filter((a) => a.status !== "cancelled");
  const totalRemaining: Amount = sum(live.map((a) => amount(a.remaining)));

  const yearOf = (yearId: string) => years.find((y) => y.id === yearId);

  return (
    <main className="screen">
      <Crumbs items={[{ label: "الطلبة", to: "/students" }, { label: view.full_name }]} />
      <div className="screen__head">
        <h1 className="screen__title">{view.full_name}</h1>
        <span className="num">{view.student_no}</span>
        <span className="grow" />
        <Link className="btn" to={`/students/${view.id}/statement`}>
          كشف الطالب
        </Link>
        {can("identity.read") && (
          <Link className="btn" to={`/students/${view.id}/identity`}>
            الهوية القانونية
          </Link>
        )}
        {can("discount.assign") && (
          <Link className="btn" to={`/students/${view.id}/discounts/new`}>
            منح خصم
          </Link>
        )}
        {can("enrollment.write") && (
          <Link className="btn" to={`/enrollments/new?student=${view.id}`}>
            تسجيل في سنة
          </Link>
        )}
        {can("payment.record") && (
          <Link className="btn btn--primary" to={`/desk?student=${view.student_no}`}>
            فتح على الشبّاك
          </Link>
        )}
      </div>

      <div className="cols cols--half" style={{ marginTop: 14 }}>
        {/* ------------------------------------------------------ identity */}
        <Panel title="الهوية">
          <Row label="الاسم الكامل">{view.full_name}</Row>
          <Row label="اسم الأم">
            <b>{view.mother_name}</b>
          </Row>
          <Row label="الرقم الجامعي">
            <span className="num">{view.student_no}</span>
          </Row>
          <Row label="الهاتف">
            <span className="num">{formatPhone(view.phone)}</span>
          </Row>
          {view.national_id && (
            <Row label="رقم وطني">
              <span className="num">{view.national_id}</span>
            </Row>
          )}
          <Row label="الحالة">{labelStudentStatus(view.status)}</Row>
        </Panel>

        {/* ------------------------------------------------------- summary */}
        <Panel title="المجموع عبر الحسابات">
          <Row label="المتبقي الكلي">
            <Money value={totalRemaining} size="huge" />
          </Row>
          {/* Labelled as an aggregation, never as a balance. */}
          <p className="note" style={{ marginTop: 8 }}>
            <Chip tone="frozen">تجميع قراءة</Chip> جُمع عبر{" "}
            <span className="num">{live.length}</span>{" "}
            {live.length === 2 ? "حسابين" : "حساباً"} — ليس رصيد حساب واحد. الوحدة المالية هي
            التسجيل لا الطالب، ولكل سنة حسابها وأسعارها وديونها.
          </p>
          {accountRows.length !== live.length && (
            <p className="note">
              وهناك <span className="num">{accountRows.length - live.length}</span> حساباً ملغى
              معروضاً أدناه — غالباً هو سبب وجود الحساب الذي خلفه.
            </p>
          )}
        </Panel>
      </div>

      {/* ------------------------------------------------- academic chain */}
      <div style={{ marginTop: 14 }}>
        <Panel title="السلسلة الأكاديمية" flush>
          {(enrollments.data ?? []).length === 0 ? (
            <EmptyState kind="not-yet" title="لا تسجيلات" />
          ) : (
            <table className="grid">
              <thead>
                <tr>
                  <th>السنة</th>
                  <th className="n">المرحلة</th>
                  <th className="n">المحاولة</th>
                  <th>النوع</th>
                  <th>النتيجة</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {(enrollments.data ?? []).map((enrollment) => (
                  <tr key={enrollment.id}>
                    <td className="num">
                      <Link to={`/enrollments/${enrollment.id}`}>
                        {yearOf(enrollment.academic_year_id)?.code ?? "—"}
                      </Link>
                    </td>
                    <td className="n">{enrollment.stage}</td>
                    <td className="n">{enrollment.attempt_number}</td>
                    <td>{labelEnrollmentKind(enrollment.kind)}</td>
                    <td>
                      {labelResult(enrollment.result)}
                      {enrollment.result_by_decision && (
                        <span className="label"> (بقرار)</span>
                      )}
                    </td>
                    <td>
                      <StateChip entity="enrollment" status={enrollment.status} />
                      {/* The lineage is what explains two rows in one year. */}
                      {enrollment.supersedes_id && (
                        <span className="label">
                          {" "}
                          ← استبدل سابقاً{enrollment.supersede_reason ? `: ${enrollment.supersede_reason}` : ""}
                        </span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>

      {/* ------------------------------------------------------- accounts */}
      <div className="stack" style={{ marginTop: 14 }}>
        {accountRows.length === 0 && (
          <EmptyState
            kind="not-yet"
            title="لا حسابات مالية"
            detail="التسجيل وحده لا يولّد حساباً — التوليد أمر منفصل."
          />
        )}
        {accountRows.map((account) => {
          const year = yearOf(account.academic_year_id);
          return (
            <Panel
              key={account.id}
              title={
                <>
                  <span className="num">{year?.code ?? "—"}</span>{" "}
                  <StateChip entity="account" status={account.status} />
                  {year && <StateChip entity="year" status={year.status} />}
                </>
              }
              aside={
                <Link className="btn btn--sm" to={`/accounts/${account.id}`}>
                  فتح الحساب
                </Link>
              }
            >
              <Waterfall account={account} />
            </Panel>
          );
        })}
      </div>

      <div style={{ marginTop: 14 }}>
        <SponsorshipPanel studentId={view.id} />
      </div>

      {/* ------------------------------------------------------- timeline */}
      <div style={{ marginTop: 14 }}>
        <Panel title="الخط الزمني" aside={<span className="label">شنو صار بهذا الملف</span>}>
          {!can("oversight.read") ? (
            <EmptyState
              kind="forbidden"
              title="سجل التدقيق للمدقق والمدير المالي"
              detail="الصراف الذي يقرأ سجل التدقيق يعرف أيضاً أي تصحيحاته لوحظ."
            />
          ) : audit.isLoading ? (
            <Skeleton height={120} />
          ) : audit.isError ? (
            <EmptyState kind="no-results" title="تعذّر جلب الخط الزمني" />
          ) : (audit.data ?? []).length === 0 ? (
            <EmptyState kind="not-yet" title="لا أحداث مسجّلة بعد" />
          ) : (
            <ul className="timeline">
              {(audit.data ?? []).map((entry, index) => (
                <li className="timeline__item" key={entry.id ?? index}>
                  <span className="timeline__dot" data-tone={toneOf(entry.action)} />
                  <span className="timeline__when">{formatDateTime(entry.occurred_at)}</span>{" "}
                  <b>{entry.action}</b>{" "}
                  {entry.actor && <span className="timeline__who">— {entry.actor}</span>}
                  {entry.entity_type && (
                    <span className="label"> · {entry.entity_type}</span>
                  )}
                </li>
              ))}
            </ul>
          )}
        </Panel>
      </div>
    </main>
  );
}

/**
 * The audit row as the timeline needs it.
 *
 * Typed loosely on purpose: the audit endpoint's exact shape is not in the
 * OpenAPI document, and inventing a strict type would turn a missing field
 * into a crash on a screen an auditor is relying on.
 */
interface AuditRow {
  id?: string;
  action: string;
  actor?: string;
  entity_type?: string;
  occurred_at: string;
}

function toneOf(action: string): string {
  const a = action.toLowerCase();
  if (a.includes("void") || a.includes("cancel") || a.includes("reject")) return "void";
  if (a.includes("request") || a.includes("pending")) return "pending";
  if (a.includes("supersede") || a.includes("snapshot") || a.includes("freeze")) return "frozen";
  if (a.includes("payment") || a.includes("post") || a.includes("settle")) return "live";
  return "muted";
}

export { format };
