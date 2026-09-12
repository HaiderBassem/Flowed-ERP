import { useMemo } from "react";
import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal } from "@/api/errors";
import type { Page, RawAmount } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { EmptyState, Panel, Skeleton, Stat } from "@/components/primitives";
import { useWorkingContext } from "@/app/working-context";
import { formatDateTime } from "@/lib/dates";
import { amount, sum, type Amount } from "@/lib/money";

interface VoidRow {
  payment_id: string;
  receipt_no?: string | null;
  student_id: string;
  student_no: string;
  full_name: string;
  amount: RawAmount;
  method_code: string;
  posted_at: string;
  voided_at: string;
  gap_hours: number;
  crossed_day: boolean;
  reason: string;
  cashier_name: string;
  requested_by?: string | null;
  executed_by?: string | null;
}

/**
 * The void register — §09.
 *
 * Ordered by **time gap descending, not by date**. Sorting by date buries the
 * suspicious case among the ordinary ones, and the whole reason this register
 * exists is to surface the pattern rather than to list events. The server
 * already returns it widest-gap-first; this screen does not re-sort it.
 *
 * The side panel breaks the voids down per cashier, which with the gap column
 * is the pair of facts an approval is worth anything without.
 */
export function VoidLogScreen() {
  const { activeYear } = useWorkingContext();

  const register = useQuery({
    queryKey: ["reports", "voids", activeYear?.id],
    queryFn: () =>
      api.get<Page<VoidRow>>("/reports/voids", {
        query: { academic_year_id: activeYear?.id, limit: 200 },
      }),
    enabled: Boolean(activeYear),
  });

  const rows = register.data?.data ?? [];

  const perCashier = useMemo(() => {
    const map = new Map<string, { count: number; total: Amount; crossed: number }>();
    for (const row of rows) {
      const current = map.get(row.cashier_name) ?? {
        count: 0,
        total: 0n as Amount,
        crossed: 0,
      };
      map.set(row.cashier_name, {
        count: current.count + 1,
        total: (current.total + amount(row.amount)) as Amount,
        crossed: current.crossed + (row.crossed_day ? 1 : 0),
      });
    }
    return [...map.entries()].sort((a, b) => b[1].count - a[1].count);
  }, [rows]);

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">سجل الإلغاءات</h1>
        {activeYear && <span className="num">{activeYear.code}</span>}
      </div>

      <div className="callout callout--note" style={{ marginTop: 10 }}>
        مرتَّب بالفجوة الزمنية تنازلياً — لا بالتاريخ. الفرز بالتاريخ يدفن الحالة المريبة وسط
        الحالات العادية، ووجود هذا السجل هو لإظهار النمط لا لسرد الأحداث.
      </div>

      {register.isLoading && <Skeleton height={220} />}

      {register.isError &&
        (isRefusal(register.error) ? (
          <RefusalPanel refusal={register.error} onRetry={() => void register.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر جلب السجل" />
        ))}

      {register.isSuccess && rows.length === 0 && (
        <Panel>
          <EmptyState kind="clean" title="لا إلغاءات في هذه السنة" />
        </Panel>
      )}

      {rows.length > 0 && (
        <>
          <div className="deck" style={{ margin: "12px 0" }}>
            <Stat value={rows.length} label="إلغاءات" tone="void" />
            <Stat
              value={<Money value={sum(rows.map((r) => amount(r.amount)))} tone="plain" />}
              label="المبلغ الملغى"
            />
            <Stat
              value={rows.filter((r) => r.crossed_day).length}
              label="عبر الأيام — كان يجب أن تكون استرجاعاً"
              tone="pending"
            />
          </div>

          <div className="cols" style={{ gridTemplateColumns: "minmax(0,2.2fr) minmax(240px,1fr)" }}>
            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th className="n">الفجوة</th>
                    <th>الوصل</th>
                    <th>الطالب</th>
                    <th className="n col-group-money">المبلغ</th>
                    <th>رُحِّل</th>
                    <th>أُلغي</th>
                    <th>الصراف</th>
                    <th>السبب</th>
                  </tr>
                </thead>
                <tbody>
                  {rows.map((row) => (
                    <tr key={row.payment_id}>
                      <td className="n">
                        <span className={row.crossed_day ? "money money--neg" : "num"}>
                          {formatGap(row.gap_hours)}
                        </span>
                        {row.crossed_day && (
                          <>
                            {" "}
                            <Chip tone="void" hint="الإلغاء عبر الأيام يُصفّي درجاً سبق عدّه">
                              عبر الأيام
                            </Chip>
                          </>
                        )}
                      </td>
                      <td>
                        <Link className="ltr num" to={`/payments/${row.payment_id}`}>
                          {row.receipt_no ?? "—"}
                        </Link>
                      </td>
                      <td>
                        <Link to={`/students/${row.student_id}`}>{row.full_name}</Link>{" "}
                        <span className="num label">{row.student_no}</span>
                      </td>
                      <td className="n col-group-money">
                        <Money value={row.amount} tone="plain" />
                      </td>
                      <td className="num">{formatDateTime(row.posted_at)}</td>
                      <td className="num">{formatDateTime(row.voided_at)}</td>
                      <td>{row.cashier_name}</td>
                      <td>{row.reason}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            <Panel title="التوزيع لكل صراف">
              {perCashier.map(([name, stats]) => (
                <div className="row" key={name}>
                  <span className="grow">{name}</span>
                  {stats.crossed > 0 && (
                    <Chip tone="pending">{stats.crossed} عبر الأيام</Chip>
                  )}
                  <span className="num">{stats.count}</span>
                  <Money value={stats.total} tone="plain" />
                </div>
              ))}
              <p className="note" style={{ marginTop: 8 }}>
                عدد إلغاءات الصراف والفجوة الزمنية هما العمودان اللذان يوجد هذا السجل لأجلهما.
                موافقة بلا هذين الرقمين موافقة على الورق فقط.
              </p>
            </Panel>
          </div>
        </>
      )}
    </main>
  );
}

function formatGap(hours: number): string {
  if (hours < 1) return `${Math.round(hours * 60)}د`;
  if (hours < 24) return `${hours.toFixed(1)}س`;
  return `${Math.floor(hours / 24)}ي ${Math.round(hours % 24)}س`;
}
