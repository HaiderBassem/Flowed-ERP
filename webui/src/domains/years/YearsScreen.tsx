import { Link } from "react-router-dom";

import { StateChip } from "@/components/Chip";
import { EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { formatDate } from "@/lib/dates";
import { labelDebtPolicy } from "@/design/lexicon";

/**
 * The academic years.
 *
 * The two closes are shown as two separate columns rather than folded into one
 * status word, because they are genuinely independent: money freezes while
 * academic recording stays open, since second-round (دور ثاني) results arrive
 * weeks after the treasury shuts its books.
 */
export function YearsScreen() {
  const { years, loading } = useWorkingContext();
  const { can } = useSession();

  if (loading) {
    return (
      <main className="screen">
        <Skeleton height={200} />
      </main>
    );
  }

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">السنوات الدراسية</h1>
        <span className="grow" />
        {can("year.administer") && (
          <Link className="btn btn--primary" to="/years/new">
            سنة جديدة
          </Link>
        )}
      </div>

      <Panel flush>
        {years.length === 0 ? (
          <EmptyState kind="not-yet" title="لا سنوات معرّفة" />
        ) : (
          <table className="grid">
            <thead>
              <tr>
                <th>السنة</th>
                <th>من</th>
                <th>إلى</th>
                <th>الحالة</th>
                <th>ترحيل مالي</th>
                <th>تسجيل أكاديمي</th>
                <th>سياسة حجب الدين</th>
              </tr>
            </thead>
            <tbody>
              {years.map((year) => (
                <tr key={year.id}>
                  <td className="k">
                    <Link to={`/years/${year.id}`}>{year.code}</Link>
                  </td>
                  <td className="num">{formatDate(year.start_date)}</td>
                  <td className="num">{formatDate(year.end_date)}</td>
                  <td>
                    <StateChip entity="year" status={year.status} />
                  </td>
                  <td>
                    {year.accepts_financial_posting ? (
                      <span style={{ color: "var(--accent)" }}>يقبل</span>
                    ) : (
                      <span style={{ color: "var(--muted)" }}>مغلق</span>
                    )}
                  </td>
                  <td>
                    {year.accepts_academic_recording ? (
                      <span style={{ color: "var(--accent)" }}>مفتوح</span>
                    ) : (
                      <span style={{ color: "var(--muted)" }}>مغلق</span>
                    )}
                  </td>
                  <td>{labelDebtPolicy(year.debt_block_policy)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <p className="note" style={{ marginTop: 10, maxWidth: "72ch" }}>
        للسنة إغلاقان لا واحد. الإغلاق المالي يجمّد المال بينما يبقى التسجيل الأكاديمي مفتوحاً،
        لأن نتائج الدور الثاني تصل بعد أسابيع من إغلاق الخزينة. وإغلاق السنة{" "}
        <b>لا يوقف تحصيل ديونها</b> — القبض يُرحَّل بوصل السنة المفتوحة ويُخصَّص لأقساط السنة
        القديمة.
      </p>
    </main>
  );
}
