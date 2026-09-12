import type { InstallmentView } from "@/api/types";
import { amount, type Amount } from "@/lib/money";

/**
 * The allocation preview.
 *
 * §08 requires the cashier to see which installments the money will land on
 * **before** posting, and to see any surplus as a credit balance rather than
 * as a surprise after the receipt has printed.
 *
 * This is the one place the interface projects a number the server has not
 * yet returned, and it needs justifying against §15's "no amount is computed
 * in the browser". Two things make it acceptable, and neither is comfort:
 *
 *   - it is a *preview of a rule*, not a figure of record. The authoritative
 *     allocation is the one the server returns from POST /payments, and that
 *     is what the receipt and the account page show afterwards. Nothing here
 *     is ever persisted or displayed as a posted figure.
 *   - the alternative is worse. Posting money and *then* showing where it went
 *     is exactly the surprise the specification is written against.
 *
 * The rule mirrored here is the server's default: oldest due date first. When
 * the cashier directs the money manually the preview is bypassed entirely and
 * the targets are sent explicitly, so the two cannot disagree.
 */

export interface PreviewLine {
  installment: InstallmentView;
  applied: Amount;
}

export interface AllocationPreview {
  lines: PreviewLine[];
  /** Money left over once every outstanding installment is covered. */
  surplus: Amount;
  /** True when the payment clears the account entirely. */
  settlesAccount: boolean;
}

export function previewAllocation(
  installments: readonly InstallmentView[],
  total: Amount,
): AllocationPreview {
  // Oldest due date first, then by number — the same order the server applies.
  const outstanding = installments
    .filter((i) => amount(i.remaining) > 0n && i.status !== "superseded")
    .slice()
    .sort((a, b) =>
      a.due_date === b.due_date ? a.number - b.number : a.due_date < b.due_date ? -1 : 1,
    );

  const lines: PreviewLine[] = [];
  let left = total;

  for (const installment of outstanding) {
    if (left <= 0n) break;
    const remaining = amount(installment.remaining);
    const applied = left < remaining ? left : remaining;
    lines.push({ installment, applied: applied as Amount });
    left = (left - applied) as Amount;
  }

  const totalOutstanding = outstanding.reduce(
    (sum, i) => (sum + amount(i.remaining)) as Amount,
    0n as Amount,
  );

  return {
    lines,
    surplus: left,
    settlesAccount: total >= totalOutstanding && totalOutstanding > 0n,
  };
}
