import { useState } from "react";
import { useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { formatDate } from "@/lib/dates";
import { labelDebtPolicy } from "@/design/lexicon";

interface ChainResult {
  intact: boolean;
  problems: unknown[];
}

/**
 * The year console — §09.
 *
 * The financial close runs off a live checklist and the button stays disabled
 * until every item is green. Closing over a difference freezes that difference
 * forever, which is the reason the checklist is a gate rather than advice.
 */
export function YearScreen() {
  const { id } = useParams<{ id: string }>();
  const { years } = useWorkingContext();
  const { can, reason } = useSession();
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const year = years.find((y) => y.id === id);

  // `clean` comes from the server rather than being inferred from an empty
  // array: the server knows whether it looked. A client that reads "no rows"
  // as "no drift" would show all-clear for a check that never ran, and the
  // close button below is gated on exactly this.
  const drift = useQuery({
    queryKey: ["oversight", "reconciliation"],
    queryFn: () =>
      api.get<{ clean: boolean; drifting_accounts: unknown[] }>("/oversight/reconciliation", {
        query: { limit: 50 },
      }),
    enabled: can("oversight.read"),
  });

  const chain = useQuery({
    queryKey: ["oversight", "audit", "verify"],
    queryFn: () => api.get<ChainResult>("/oversight/audit/verify"),
    enabled: can("oversight.read"),
    staleTime: Infinity,
  });

  const act = useMutation({
    mutationFn: (action: "open" | "close-financially" | "close") =>
      api.post(`/academic-years/${id}/${action}`, {}),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["academic-years"] });
      setRefusal(null);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (!year) {
    return (
      <main className="screen">
        <EmptyState kind="no-results" title="سنة غير معروفة" />
      </main>
    );
  }

  const driftClean = drift.isSuccess && drift.data!.clean;
  const chainClean = chain.data?.intact === true;
  const checksReady = driftClean && chainClean;

  const checks: { label: string; state: "met" | "blocked" | "checking"; detail: string }[] = [
    {
      label: "مصالحة صفرية",
      state: drift.isLoading ? "checking" : driftClean ? "met" : "blocked",
      detail: driftClean
        ? "الأرصدة المخزّنة تطابق المحسوبة"
        : `انحراف على ${drift.data?.drifting_accounts.length ?? 0} حساباً — الإغلاق فوق فرق يجمّده للأبد`,
    },
    {
      label: "سلسلة التدقيق سليمة",
      state: chain.isLoading ? "checking" : chainClean ? "met" : "blocked",
      detail: chainClean ? "كل قيد يشير إلى سلفه بتجزئة صحيحة" : "السلسلة مكسورة — راجع صفحة التدقيق",
    },
  ];

  return (
    <main className="screen">
      <Crumbs items={[{ label: "السنوات الدراسية", to: "/years" }, { label: year.code }]} />
      <div className="screen__head">
        <h1 className="screen__title">
          كونسول السنة <span className="num">{year.code}</span>
        </h1>
        <StateChip entity="year" status={year.status} />
      </div>

      <div className="cols cols--half" style={{ marginTop: 14 }}>
        <Panel title="الحالة">
          <Row label="من">
            <span className="num">{formatDate(year.start_date)}</span>
          </Row>
          <Row label="إلى">
            <span className="num">{formatDate(year.end_date)}</span>
          </Row>
          <Row label="الترحيل المالي">
            {year.accepts_financial_posting ? "مفتوح" : "مغلق"}
          </Row>
          <Row label="التسجيل الأكاديمي">
            {year.accepts_academic_recording ? "مفتوح" : "مغلق"}
          </Row>
          <Row label="سياسة حجب الدين">{labelDebtPolicy(year.debt_block_policy)}</Row>
        </Panel>

        <Panel title="قائمة فحص الإغلاق المالي">
          {checks.map((check) => (
            <div className="precondition" data-state={check.state} key={check.label}>
              <span className="precondition__mark">
                {check.state === "met" ? "✓" : check.state === "checking" ? "…" : "✕"}
              </span>
              <span className="grow">
                <b>{check.label}</b>
                <div className="note">{check.detail}</div>
              </span>
            </div>
          ))}

          {refusal && (
            <div style={{ marginTop: 10 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <div className="cluster" style={{ marginTop: 12 }}>
            <Button
              variant="primary"
              disabled={!can("year.administer") || !checksReady || !year.accepts_financial_posting}
              disabledReason={
                !can("year.administer")
                  ? reason("year.administer")
                  : !year.accepts_financial_posting
                    ? "مغلقة مالياً أصلاً"
                    : !checksReady
                      ? "الزر معطّل حتى تخضرّ كل بنود الفحص"
                      : undefined
              }
              busy={act.isPending}
              onClick={() => act.mutate("close-financially")}
            >
              إغلاق مالي
            </Button>
            <Button
              disabled={!can("year.administer") || year.status !== "financially_closed"}
              disabledReason={
                !can("year.administer")
                  ? reason("year.administer")
                  : year.status !== "financially_closed"
                    ? "الإغلاق الكامل يتطلّب إغلاقاً مالياً أولاً"
                    : undefined
              }
              busy={act.isPending}
              onClick={() => act.mutate("close")}
            >
              إغلاق كامل
            </Button>
            {year.status === "draft" && (
              <Button
                disabled={!can("year.administer")}
                disabledReason={reason("year.administer")}
                onClick={() => act.mutate("open")}
              >
                فتح السنة
              </Button>
            )}
          </div>

          <p className="note" style={{ marginTop: 10 }}>
            إغلاق السنة لا يوقف تحصيل ديونها: القبض يُرحَّل بوصل السنة المفتوحة ويُخصَّص لأقساط
            هذه السنة.
          </p>
        </Panel>
      </div>
    </main>
  );
}
