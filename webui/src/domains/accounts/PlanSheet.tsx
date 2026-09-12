import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type {
  AdjustPlanResponse,
  InstallmentView,
  PlanRevisionView,
} from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { formatDate, formatDateTime } from "@/lib/dates";
import { amount } from "@/lib/money";

/**
 * Rescheduling and re-splitting an installment plan.
 *
 * Two different acts with two different blast radii, kept visibly apart:
 * a **reschedule** moves due dates and touches nothing financial; a
 * **resplit** redivides the *unpaid remainder* into new installments. Paid
 * installments are untouchable in both — the money that landed on them has
 * receipts.
 *
 * Every change is a recorded revision with before/after, listed below,
 * because a payment plan that changed with no trace is how a student and a
 * cashier end up arguing over which schedule is real.
 */
export function PlanSheet({
  accountId,
  installments,
  onDone,
}: {
  accountId: string;
  installments: InstallmentView[];
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [kind, setKind] = useState<"reschedule" | "resplit">("reschedule");
  const [reason, setReason] = useState("");
  const [dueDates, setDueDates] = useState<Record<string, string>>({});
  const [shares, setShares] = useState<{ percent: string; offset: string; label: string }[]>([
    { percent: "50", offset: "0", label: "" },
    { percent: "50", offset: "60", label: "" },
  ]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [result, setResult] = useState<AdjustPlanResponse | null>(null);

  const unpaid = installments.filter(
    (i) => i.status !== "paid" && i.status !== "superseded" && amount(i.remaining) > 0n,
  );

  const totalBp = useMemo(
    () =>
      shares.reduce((sum, share) => {
        const pct = Number(share.percent);
        return sum + (Number.isFinite(pct) ? Math.round(pct * 100) : 0);
      }, 0),
    [shares],
  );
  const balanced = totalBp === 10000;

  const adjust = useMutation({
    mutationFn: () =>
      api.post<AdjustPlanResponse>(`/accounts/${accountId}/plan`, {
        kind,
        reason: reason.trim(),
        ...(kind === "reschedule" ? { due_dates: dueDates } : {}),
        ...(kind === "resplit"
          ? {
              shares: shares.map((share) => ({
                share_bp: Math.round(Number(share.percent) * 100),
                due_offset_days: Number(share.offset) || 0,
                ...(share.label.trim() ? { label: share.label.trim() } : {}),
              })),
            }
          : {}),
      }),
    onSuccess: (response) => {
      setResult(response);
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["account", accountId] });
      void queryClient.invalidateQueries({ queryKey: ["plan-revisions", accountId] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const movedDates = Object.keys(dueDates).length;
  const ready =
    reason.trim().length >= 3 &&
    (kind === "reschedule" ? movedDates > 0 : balanced && shares.length > 0);

  if (result) {
    return (
      <Panel title="عُدِّلت الخطة">
        {result.revision && (
          <p className="note" style={{ marginBottom: 8 }}>
            المراجعة <b className="num">{result.revision.plan_version}</b>: من{" "}
            <b className="num">{result.revision.installments_before}</b> قسطاً إلى{" "}
            <b className="num">{result.revision.installments_after}</b> — غير المدفوع{" "}
            <Money value={result.revision.unpaid_before} tone="plain" /> قبلها وبقي{" "}
            <Money value={result.revision.unpaid_after} tone="plain" /> بعدها، بالدينار نفسه.
          </p>
        )}
        <Button variant="ghost" onClick={onDone}>
          إغلاق
        </Button>
      </Panel>
    );
  }

  return (
    <Panel title="تعديل خطة الأقساط">
      <div className="cluster" style={{ marginBottom: 12 }}>
        <button
          type="button"
          className={`btn${kind === "reschedule" ? " btn--primary" : ""}`}
          onClick={() => setKind("reschedule")}
        >
          إعادة جدولة — تواريخ فقط
        </button>
        <button
          type="button"
          className={`btn${kind === "resplit" ? " btn--primary" : ""}`}
          onClick={() => setKind("resplit")}
        >
          إعادة تقسيم غير المدفوع
        </button>
      </div>

      {unpaid.length === 0 ? (
        <EmptyState kind="not-yet" title="لا أقساط غير مدفوعة — لا شيء يُعدَّل" />
      ) : kind === "reschedule" ? (
        <>
          <p className="note" style={{ marginBottom: 8 }}>
            حرّك تواريخ الاستحقاق وحدها. المبالغ لا تُمسّ، والمدفوع لا يظهر هنا أصلاً.
          </p>
          <table className="grid" style={{ marginBottom: 10 }}>
            <thead>
              <tr>
                <th className="n">#</th>
                <th className="n col-group-money">المتبقي</th>
                <th>الاستحقاق الحالي</th>
                <th>الاستحقاق الجديد</th>
              </tr>
            </thead>
            <tbody>
              {unpaid.map((installment) => (
                <tr key={installment.id}>
                  <td className="n">{installment.number}</td>
                  <td className="n col-group-money">
                    <Money value={installment.remaining} tone="plain" />
                  </td>
                  <td className="num">{formatDate(installment.due_date)}</td>
                  <td>
                    <input
                      className="input num"
                      type="date"
                      value={dueDates[installment.id] ?? ""}
                      onChange={(e) =>
                        setDueDates((current) => {
                          const next = { ...current };
                          if (e.target.value) next[installment.id] = e.target.value;
                          else delete next[installment.id];
                          return next;
                        })
                      }
                    />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </>
      ) : (
        <>
          <p className="note" style={{ marginBottom: 8 }}>
            يقسم غير المدفوع —{" "}
            <Money
              value={unpaid.reduce((sum, i) => sum + i.remaining, 0)}
              tone="plain"
            />{" "}
            — على حصص جديدة. الأقساط المدفوعة تبقى كما هي: عليها وصولات.
          </p>
          <table className="grid" style={{ marginBottom: 10 }}>
            <thead>
              <tr>
                <th className="n">#</th>
                <th>الوصف</th>
                <th className="n">الحصة ٪</th>
                <th className="n">الاستحقاق بعد (يوم)</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {shares.map((share, index) => (
                <tr key={index}>
                  <td className="n">{index + 1}</td>
                  <td>
                    <input
                      className="input"
                      value={share.label}
                      onChange={(e) =>
                        setShares((c) =>
                          c.map((s, i) => (i === index ? { ...s, label: e.target.value } : s)),
                        )
                      }
                    />
                  </td>
                  <td className="n">
                    <input
                      className="input ltr"
                      style={{ textAlign: "end" }}
                      inputMode="decimal"
                      value={share.percent}
                      onChange={(e) =>
                        setShares((c) =>
                          c.map((s, i) => (i === index ? { ...s, percent: e.target.value } : s)),
                        )
                      }
                    />
                  </td>
                  <td className="n">
                    <input
                      className="input ltr"
                      style={{ textAlign: "end" }}
                      inputMode="numeric"
                      value={share.offset}
                      onChange={(e) =>
                        setShares((c) =>
                          c.map((s, i) => (i === index ? { ...s, offset: e.target.value } : s)),
                        )
                      }
                    />
                  </td>
                  <td>
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={shares.length <= 1}
                      onClick={() => setShares((c) => c.filter((_, i) => i !== index))}
                    >
                      حذف
                    </Button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          <div className="cluster" style={{ marginBottom: 10 }}>
            <Button
              size="sm"
              onClick={() => setShares((c) => [...c, { percent: "0", offset: "0", label: "" }])}
            >
              + حصة
            </Button>
            <span className="grow" />
            {balanced ? (
              <Chip tone="live">متوازن — 100٪</Chip>
            ) : (
              <Chip tone="void">المجموع {(totalBp / 100).toFixed(2)}٪ — يجب 100٪</Chip>
            )}
          </div>
        </>
      )}

      <label className="field">
        <span className="field__label">السبب (إلزامي، ويبقى في سجل المراجعات)</span>
        <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!ready}
          disabledReason={
            reason.trim().length < 3
              ? "اكتب سبباً"
              : kind === "reschedule"
                ? "غيّر تاريخاً واحداً على الأقل"
                : "الحصص يجب أن تبلغ 100٪"
          }
          busy={adjust.isPending}
          onClick={() => adjust.mutate()}
        >
          تنفيذ التعديل
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}

/** The recorded revisions — the paper trail of every plan change. */
export function PlanRevisions({ accountId }: { accountId: string }) {
  const revisions = useQuery({
    queryKey: ["plan-revisions", accountId],
    queryFn: () => api.get<PlanRevisionView[]>(`/accounts/${accountId}/plan-revisions`),
  });

  if (revisions.isLoading) return <Skeleton height={80} />;
  const rows = revisions.data ?? [];
  if (rows.length === 0) {
    return (
      <p className="note" style={{ padding: "10px 14px" }}>
        الخطة كما وُلدت — لا مراجعات.
      </p>
    );
  }

  return (
    <ul className="timeline" style={{ padding: "10px 14px" }}>
      {rows.map((revision) => (
        <li className="timeline__item" key={revision.id}>
          <span className="timeline__dot" data-tone="frozen" />
          <span className="timeline__when">{formatDateTime(revision.created_at)}</span>{" "}
          <b>{revision.kind === "reschedule" ? "إعادة جدولة" : "إعادة تقسيم"}</b>{" "}
          <span className="label">
            v{revision.plan_version} · من {revision.installments_before} إلى{" "}
            {revision.installments_after} قسطاً · غير المدفوع{" "}
          </span>
          <Money value={revision.unpaid_before} tone="plain" />
          <span className="label"> ← </span>
          <Money value={revision.unpaid_after} tone="plain" />
          <div className="note">«{revision.reason}»</div>
        </li>
      ))}
    </ul>
  );
}
