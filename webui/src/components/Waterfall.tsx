import type { ReactNode } from "react";

import { Chip } from "./Chip";
import { Money } from "./Money";
import type { AccountView, DiscountApplicationView, SnapshotLineView } from "@/api/types";
import { amount } from "@/lib/money";

/**
 * The financial waterfall — §07 pattern two.
 *
 * This is the visual form of the system's central equation, and it appears
 * identically in the account page, the student statement and the pricing
 * preview. The sameness is deliberate: an operator learns it once.
 *
 *   gross → discounts → FROZEN NET → Σ adjustments → EFFECTIVE NET
 *
 * Showing only "outstanding: 1,000,000" leaves "why was it 2,000,000?"
 * unanswerable from the screen when the answer is sitting in the rows. The
 * standard §01 sets is a ministry auditor standing behind the clerk being able
 * to read from the screen alone why this number is this number.
 *
 * Every line here corresponds to a real row, and `onOpenSource` is how a line
 * opens the row it came from.
 */
export function Waterfall({
  account,
  components,
  discounts,
  onOpenSource,
}: {
  account: AccountView;
  components?: SnapshotLineView[];
  discounts?: DiscountApplicationView[];
  onOpenSource?: (source: { kind: "component" | "discount" | "adjustment"; id: string }) => void;
}) {
  const adjustments = amount(account.adjustment_total);

  return (
    <div className="waterfall">
      <Line
        label={
          <>
            إجمالي الرسوم <Chip tone="frozen" hint="نُسخت من الإعدادات لحظة التوليد ولا يعاد حسابها">مجمّد</Chip>
          </>
        }
        value={<Money value={account.gross_total} size="big" tone="plain" />}
      />

      {components?.map((line) => (
        <Line
          key={line.component_code}
          sub
          label={
            <button
              type="button"
              className="waterfall__source"
              onClick={() => onOpenSource?.({ kind: "component", id: line.component_code })}
            >
              <span className="label">
                — {line.name_ar}{" "}
                {line.is_discountable ? "(قابل للخصم)" : "(غير قابل للخصم)"}
              </span>
            </button>
          }
          value={<Money value={line.amount} tone="plain" />}
        />
      ))}

      <Line
        label={
          <>
            الخصومات{" "}
            <span className="label">
              (من الأساس القابل للخصم <Money value={account.discountable_base} tone="plain" />)
            </span>
          </>
        }
        value={<Money value={negate(account.discount_total)} />}
      />

      {discounts?.map((d) => (
        <Line
          key={d.id}
          sub
          label={
            <button
              type="button"
              className="waterfall__source"
              onClick={() => onOpenSource?.({ kind: "discount", id: d.id })}
            >
              <span className="label">— تطبيق خصم</span>{" "}
              <Chip tone="frozen" hint="مربوط بإصدار التعريف الذي كان سارياً وقتها">
                مجمّد
              </Chip>
              {/* A truncated grant keeps both figures and names the reason, so
                  nothing is reduced silently. */}
              {d.truncation_reason && (
                <span className="label">
                  {" "}
                  · احتُسب <Money value={d.computed_amount} tone="plain" /> وقُصّ:{" "}
                  {d.truncation_reason}
                </span>
              )}
            </button>
          }
          value={<Money value={negate(d.applied_amount)} />}
        />
      ))}

      {/* Set once at generation and never moved again — every later change is
          a signed adjustment row below it. */}
      <Line
        variant="frozen"
        label={
          <>
            <b>الصافي المجمّد</b> <span className="label">لا يتغيّر أبداً</span>
          </>
        }
        value={<Money value={account.net_snapshot} size="big" tone="plain" />}
      />

      <Line
        label={
          <>
            قيود التسوية{" "}
            <span className="label">
              {adjustments === 0n ? "(لا يوجد)" : "(مجموع القيود الموقّعة)"}
            </span>
          </>
        }
        value={<Money value={account.adjustment_total} sign={adjustments === 0n ? "auto" : "always"} />}
      />

      <Line
        variant="effective"
        label={
          <>
            <b style={{ color: "var(--accent)" }}>الصافي الفعّال</b>{" "}
            <span className="label">= المجمّد + Σ القيود</span>
          </>
        }
        value={<Money value={account.effective_net} size="big" tone="positive" />}
      />

      <PaidBar account={account} />
    </div>
  );
}

function Line({
  label,
  value,
  sub,
  variant,
}: {
  label: ReactNode;
  value: ReactNode;
  sub?: boolean;
  variant?: "frozen" | "effective";
}) {
  const classes = ["waterfall__line"];
  if (sub) classes.push("waterfall__line--sub");
  if (variant) classes.push(`waterfall__line--${variant}`);
  return (
    <div className={classes.join(" ")}>
      <div className="grow">{label}</div>
      {value}
    </div>
  );
}

/**
 * Paid / outstanding, with the credit balance shown rather than folded away.
 *
 * The bar follows reading direction because it is a narrative, not a time
 * axis — §06 is explicit that the two mirror differently.
 */
function PaidBar({ account }: { account: AccountView }) {
  const net = amount(account.effective_net);
  const paid = amount(account.net_paid);
  const remaining = amount(account.remaining);
  const credit = amount(account.credit_balance);

  const pct = (part: bigint): number => {
    if (net <= 0n) return 0;
    // Percent of the bar only; no money is computed here. Integer arithmetic
    // on bigints, then to a number purely for CSS width.
    const scaled = (part * 1000n) / net;
    return Math.min(100, Math.max(0, Number(scaled) / 10));
  };

  return (
    <div style={{ marginTop: 12 }}>
      <div className="bar" role="img" aria-label={`مدفوع ${pct(paid).toFixed(0)}٪ من الصافي الفعّال`}>
        <i style={{ width: `${pct(paid)}%`, background: "var(--accent)" }} />
        <i style={{ width: `${pct(remaining)}%`, background: "var(--surface-3)" }} />
      </div>
      <div className="cluster" style={{ marginTop: 8 }}>
        <span className="label">
          مدفوع صافي <Money value={account.net_paid} tone="positive" />
        </span>
        {amount(account.refunded_total) > 0n && (
          <span className="label">
            مسترجَع <Money value={negate(account.refunded_total)} />
          </span>
        )}
        <span className="grow" />
        {credit > 0n && (
          <span className="label">
            رصيد دائن <Money value={account.credit_balance} tone="positive" />
          </span>
        )}
        <span className="label">
          المتبقي <Money value={account.remaining} size="big" tone="plain" />
        </span>
      </div>
    </div>
  );
}

/** Display-only negation: these figures are shown as deductions, not recomputed. */
function negate(raw: number): bigint {
  return -amount(raw);
}
