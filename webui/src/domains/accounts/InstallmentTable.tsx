import { useEffect, useState } from "react";

import type { InstallmentView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { EmptyState } from "@/components/primitives";
import { daysOverdue, formatDate, today } from "@/lib/dates";
import { overdueLabel } from "@/design/lexicon";

/**
 * The installment plan.
 *
 * Overdue is derived here, at the moment of display, because it is not a
 * stored state: it is `due_date < today` with money outstanding. The server
 * sends is_overdue computed for the request, and the day count is recomputed
 * on screen so that an installment which fell due at midnight reads as overdue
 * in the morning **without a reload** — a desk screen is left open for eight
 * hours, and a stale "due" beside an overdue row is a wrong answer given to a
 * student.
 */
export function InstallmentTable({
  installments,
  compact,
}: {
  installments: InstallmentView[];
  compact?: boolean;
}) {
  const reference = useToday();

  if (installments.length === 0) {
    return <EmptyState kind="not-yet" title="لا خطة أقساط على هذا الحساب" />;
  }

  return (
    <div className="table-wrap" style={{ border: 0 }}>
      <table className="grid">
        <thead>
          <tr>
            <th className="n">#</th>
            <th>الاستحقاق</th>
            <th className="n">المبلغ</th>
            <th className="n">المدفوع</th>
            <th className="n">المتبقي</th>
            <th>الحالة</th>
          </tr>
        </thead>
        <tbody>
          {installments.map((installment) => {
            const late = installment.is_overdue ? daysOverdue(installment.due_date, reference) : 0;
            return (
              <tr key={installment.id}>
                <td className="n">{installment.number}</td>
                <td className="num">{formatDate(installment.due_date)}</td>
                <td className="n">
                  <Money value={installment.amount} tone="plain" />
                </td>
                <td className="n">
                  <Money value={installment.paid_amount} tone="plain" />
                </td>
                <td className="n">
                  <Money value={installment.remaining} />
                </td>
                <td>
                  {installment.is_overdue ? (
                    <Chip tone="void" hint={overdueLabel(late).hint}>
                      {late > 0 ? `متأخر ${late} يوم` : "متأخر"}
                    </Chip>
                  ) : (
                    <StateChip entity="installment" status={installment.status} />
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
      {!compact && (
        <p className="note" style={{ padding: "8px 12px" }}>
          «متأخر» ليس حالة مخزّنة — يُشتق من تاريخ الاستحقاق لحظة العرض، ويُعاد حسابه عند تغيّر
          اليوم بلا إعادة تحميل.
        </p>
      )}
    </div>
  );
}

/**
 * The current date, re-evaluated when it changes.
 *
 * A window screen stays open across midnight. Without this the overdue column
 * would keep yesterday's answer until somebody refreshed.
 */
export function useToday(): Date {
  const [date, setDate] = useState(today);

  useEffect(() => {
    const id = window.setInterval(() => {
      const now = today();
      setDate((current) => (current.getTime() === now.getTime() ? current : now));
    }, 60_000);
    return () => window.clearInterval(id);
  }, []);

  return date;
}
