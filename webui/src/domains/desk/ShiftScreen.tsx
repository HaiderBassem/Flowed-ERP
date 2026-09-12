import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { CashierSessionSummaryView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Money, MoneyField } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { duration, formatDateTime } from "@/lib/dates";
import { amount, parseInput, type Amount } from "@/lib/money";

/**
 * The shift sheet and drawer reconciliation — §09.
 *
 * The expected figure is **computed from the rows**, never typed: a number
 * written by the person being reconciled is not a number. The variance is
 * computed live as the count is entered, and a non-zero variance opens a
 * mandatory reason field.
 *
 * Cash and non-cash sit in separate rows because only the cash rows reconcile
 * against the drawer. Mixing them manufactures a phantom variance every
 * evening, which is how a real one stops being noticed.
 */
export function ShiftScreen() {
  const { shift } = useWorkingContext();
  const { can, reason } = useSession();
  const queryClient = useQueryClient();

  const [counted, setCounted] = useState("");
  const [varianceReason, setVarianceReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const summary = useQuery({
    queryKey: ["cashier-session", shift?.id, "summary"],
    queryFn: () => api.get<CashierSessionSummaryView>(`/cashier-sessions/${shift!.id}`),
    enabled: Boolean(shift),
    refetchInterval: 30_000,
  });

  const close = useMutation({
    mutationFn: () =>
      api.post(`/cashier-sessions/${shift!.id}/close`, {
        counted_cash: Number(countedAmount),
        ...(variance !== 0n ? { variance_reason: varianceReason.trim() } : {}),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["cashier-session"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const approve = useMutation({
    mutationFn: () => api.post(`/cashier-sessions/${shift!.id}/approve`, {}),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["cashier-session"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const parsed = parseInput(counted);
  const countedAmount: Amount = parsed.ok ? parsed.value : (0n as Amount);
  const expected = amount(summary.data?.expected_cash ?? 0);

  // Displayed difference only — the authoritative variance is the server's,
  // recorded on the closed session.
  const variance = useMemo(
    () => (parsed.ok ? ((countedAmount - expected) as Amount) : (0n as Amount)),
    [parsed.ok, countedAmount, expected],
  );

  if (!shift) {
    return (
      <main className="screen">
        <div className="screen__head">
          <h1 className="screen__title">الوردية</h1>
        </div>
        <EmptyState
          kind="not-yet"
          title="لا وردية مفتوحة"
          detail="لا يمكن القبض قبل فتح وردية."
          action={
            can("shift.open") ? (
              <Link className="btn btn--primary" to="/desk/session/open">
                فتح وردية
              </Link>
            ) : null
          }
        />
      </main>
    );
  }

  if (summary.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="30%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  const sheet = summary.data;
  const closed = shift.status !== "open";
  const needsReason = variance !== 0n;

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">كشف الوردية</h1>
        <StateChip entity="shift" status={shift.status} />
        <span className="grow" />
        {shift.status === "open" && (
          <span className="label num">مفتوحة منذ {duration(shift.opened_at)}</span>
        )}
      </div>

      <div className="deck" style={{ marginTop: 12 }}>
        <Stat
          value={<Money value={sheet?.opening_float ?? 0} tone="plain" />}
          label="الرصيد الافتتاحي"
        />
        <Stat
          value={<Money value={sheet?.expected_cash ?? 0} tone="plain" />}
          label="المتوقع نقداً — محسوب من الصفوف"
          tone="live"
        />
        <Stat value={sheet?.refunds.count ?? 0} label="استرجاعات في الوردية" />
        <Stat value={sheet?.voids.count ?? 0} label="إلغاءات في الوردية" tone="void" />
      </div>

      <div className="cols cols--half" style={{ marginTop: 14 }}>
        <Panel title="التحصيل حسب الطريقة" flush>
          {(sheet?.payments_by_method ?? []).length === 0 ? (
            <EmptyState kind="not-yet" title="لم يُقبض شيء في هذه الوردية" />
          ) : (
            <table className="grid">
              <thead>
                <tr>
                  <th>الطريقة</th>
                  <th className="n">العدد</th>
                  <th className="n col-group-money">المجموع</th>
                  <th className="n">ملغى</th>
                </tr>
              </thead>
              <tbody>
                {(sheet?.payments_by_method ?? []).map((row) => (
                  <tr key={row.method_code}>
                    <td>
                      {row.method_code}{" "}
                      {row.is_cash ? (
                        <Chip tone="live" hint="هذه وحدها تُصالَح على الدرج">
                          نقد
                        </Chip>
                      ) : (
                        <Chip tone="muted" hint="لا تدخل مصالحة الدرج">
                          غير نقد
                        </Chip>
                      )}
                    </td>
                    <td className="n">{row.count}</td>
                    <td className="n col-group-money">
                      <Money value={row.total} tone="plain" />
                    </td>
                    <td className="n">
                      {row.void_count > 0 ? (
                        <Money value={row.void_total} />
                      ) : (
                        <span className="label">—</span>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
          <p className="note" style={{ padding: "8px 12px" }}>
            الدرج يُصالَح على صفوف النقد وحدها. خلط النقد بغيره يصنع فرقاً وهمياً كل مساء.
          </p>
        </Panel>

        <div className="stack">
          {!closed ? (
            <Panel title="إغلاق الوردية ومصالحة الدرج">
              <Row label="المتوقع نقداً (محسوب)">
                <Money value={sheet?.expected_cash ?? 0} tone="plain" />
              </Row>

              <MoneyField
                label="المعدود في الدرج"
                value={counted}
                onChange={setCounted}
                disabled={!can("shift.close")}
              />

              {parsed.ok && (
                <Row label="الفرق">
                  <Money value={variance} sign="always" />
                </Row>
              )}

              {/* A non-zero variance requires a reason. The field appears when
                  the variance does, because the variance is not known until
                  the drawer is totalled. */}
              {needsReason && (
                <label className="field">
                  <span className="field__label">سبب الفرق (إلزامي)</span>
                  <input
                    className="input"
                    value={varianceReason}
                    onChange={(e) => setVarianceReason(e.target.value)}
                  />
                </label>
              )}

              {refusal && (
                <div style={{ marginBottom: 10 }}>
                  <RefusalPanel refusal={refusal} />
                </div>
              )}

              <Button
                variant="primary"
                disabled={!can("shift.close") || !parsed.ok || (needsReason && varianceReason.trim().length < 3)}
                disabledReason={
                  !can("shift.close")
                    ? reason("shift.close")
                    : !parsed.ok
                      ? "أدخل المعدود"
                      : needsReason
                        ? "اكتب سبب الفرق"
                        : undefined
                }
                busy={close.isPending}
                onClick={() => close.mutate()}
              >
                إغلاق الوردية
              </Button>
            </Panel>
          ) : (
            <Panel title="الوردية مغلقة">
              <Row label="أُغلقت">
                <span className="num">{formatDateTime(shift.closed_at ?? null)}</span>
              </Row>
              <Row label="المعدود">
                <Money value={sheet?.counted_cash ?? null} tone="plain" />
              </Row>
              <Row label="الفرق">
                <Money value={sheet?.variance ?? null} sign="always" />
              </Row>
              {sheet?.variance_reason && <Row label="السبب">{sheet.variance_reason}</Row>}

              {refusal && (
                <div style={{ margin: "10px 0" }}>
                  <RefusalPanel refusal={refusal} />
                </div>
              )}

              <div className="cluster" style={{ marginTop: 10 }}>
                <Button
                  variant="primary"
                  disabled={!can("shift.approve") || shift.status === "approved"}
                  // Not a permission error but the architecture, and said that
                  // way: "you lack permission" invites a request for it.
                  disabledReason={
                    shift.status === "approved"
                      ? "معتمدة أصلاً"
                      : reason("shift.approve")
                  }
                  busy={approve.isPending}
                  onClick={() => approve.mutate()}
                >
                  اعتماد الوردية
                </Button>
              </div>
              <p className="note" style={{ marginTop: 8 }}>
                الصراف لا يعتمد درجه. هذه حقيقة معمارية في فصل الواجبات، لا نقص صلاحية.
              </p>
            </Panel>
          )}
        </div>
      </div>
    </main>
  );
}
