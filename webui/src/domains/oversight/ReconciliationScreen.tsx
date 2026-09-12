import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { ReconciliationFindingView, ReconciliationRunView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { elapsed, formatDateTime } from "@/lib/dates";

interface DriftRow {
  account_id: string;
  cached_paid: number;
  computed_paid: number;
  cached_refunded: number;
  computed_refunded: number;
  cached_credit: number;
  computed_credit: number;
}

interface DriftResult {
  clean: boolean;
  drifting_accounts: DriftRow[];
}

type Tab = "state" | "findings" | "runs";

/**
 * Reconciliation — an operation, not a log line.
 *
 * Three views of the same discipline. **الحالة** is the instantaneous answer —
 * do the cached balances match what the rows compute, right now. **الملاحظات**
 * is the ledger of violations: a finding stays one row for as long as it
 * survives, escalates on its third sighting, closes itself when a pass stops
 * seeing it, and requires a written resolution when a person closes it.
 * **الفحوصات** is the proof the checks ran — a check that stopped running
 * looks exactly like a system with nothing wrong, which is why every pass is
 * recorded whether or not it found anything.
 *
 * The remedy for a drifting cache is never an UPDATE to the cache; it is
 * finding the command that failed to maintain it. The screen says so.
 */
export function ReconciliationScreen() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) ?? "state";
  const { can, reason } = useSession();
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const setTab = (next: Tab) => {
    const p = new URLSearchParams(params);
    if (next === "state") p.delete("tab");
    else p.set("tab", next);
    setParams(p, { replace: true });
  };

  const runNow = useMutation({
    mutationFn: () => api.post("/reconciliation/run", {}),
    onSuccess: () => {
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["reconciliation"] });
      void queryClient.invalidateQueries({ queryKey: ["oversight"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">المصالحة</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("oversight.read")}
          disabledReason={reason("oversight.read")}
          busy={runNow.isPending}
          onClick={() => runNow.mutate()}
          title="يشغّل الفحوصات الأربعة الآن — لمن يوشك أن يغلق الدفاتر ولا ينتظر الفحص الليلي"
        >
          تشغيل الفحوصات الآن
        </Button>
      </div>

      {refusal && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster" style={{ marginBottom: 12 }}>
        {(
          [
            ["state", "الحالة الآن"],
            ["findings", "الملاحظات"],
            ["runs", "الفحوصات"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            className="sheet__stage"
            data-state={tab === id ? "current" : undefined}
            style={{ border: 0, background: "transparent", cursor: "pointer" }}
            onClick={() => setTab(id)}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "state" && <DriftTab />}
      {tab === "findings" && <FindingsTab />}
      {tab === "runs" && <RunsTab />}
    </main>
  );
}

/* -------------------------------------------------------------------- drift */

function DriftTab() {
  const drift = useQuery({
    queryKey: ["oversight", "reconciliation"],
    queryFn: () => api.get<DriftResult>("/oversight/reconciliation", { query: { limit: 200 } }),
    refetchInterval: 300_000,
  });

  const rows = drift.data?.drifting_accounts ?? [];

  if (drift.isLoading) return <Skeleton height={160} />;

  if (drift.isError) {
    return isRefusal(drift.error) ? (
      <RefusalPanel refusal={drift.error} onRetry={() => void drift.refetch()} />
    ) : (
      <EmptyState kind="no-results" title="تعذّر تشغيل الفحص" />
    );
  }

  if (drift.data!.clean) {
    return (
      <Panel>
        <EmptyState
          kind="clean"
          title="لا انحراف — الأرصدة المخزّنة تطابق المحسوبة"
          detail={
            <span className="note">
              آخر قراءة{" "}
              {drift.dataUpdatedAt ? `قبل ${elapsed(new Date(drift.dataUpdatedAt))}` : "الآن"}.
              الفراغ هنا هو النتيجة المطلوبة لا نقص بيانات.
            </span>
          }
        />
      </Panel>
    );
  }

  return (
    <>
      <div className="refusal" style={{ marginBottom: 12 }}>
        <p className="refusal__title">انحراف على {rows.length} حساباً</p>
        <p className="refusal__body" style={{ margin: 0 }}>
          الرصيد المخزّن لا يطابق ما تحسبه الصفوف. العلاج ليس تحديث الرصيد المخزّن أبداً — بل
          إيجاد الأمر الذي أخفق في صيانته. إغلاق سنة فوق فرق يجمّده للأبد.
        </p>
      </div>
      <div className="table-wrap">
        <table className="grid">
          <thead>
            <tr>
              <th>الحساب</th>
              <th className="n col-group-money">مدفوع مخزّن</th>
              <th className="n">مدفوع محسوب</th>
              <th className="n">مسترجَع مخزّن</th>
              <th className="n">مسترجَع محسوب</th>
              <th className="n">دائن مخزّن</th>
              <th className="n">دائن محسوب</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.account_id}>
                <td className="k">
                  <Link to={`/accounts/${row.account_id}`}>فتح الحساب</Link>
                </td>
                <td className="n col-group-money">
                  <Money value={row.cached_paid} tone="plain" />
                </td>
                <td className="n">
                  <Money
                    value={row.computed_paid}
                    tone={row.cached_paid === row.computed_paid ? "plain" : "auto"}
                  />
                </td>
                <td className="n">
                  <Money value={row.cached_refunded} tone="plain" />
                </td>
                <td className="n">
                  <Money value={row.computed_refunded} tone="plain" />
                </td>
                <td className="n">
                  <Money value={row.cached_credit} tone="plain" />
                </td>
                <td className="n">
                  <Money value={row.computed_credit} tone="plain" />
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </>
  );
}

/* ----------------------------------------------------------------- findings */

function FindingsTab() {
  const [state, setState] = useState("open");

  const findings = useQuery({
    queryKey: ["reconciliation", "findings", state],
    queryFn: () =>
      api.get<ReconciliationFindingView[]>("/reconciliation/findings", {
        query: { state: state || undefined, limit: 100 },
      }),
  });

  const rows = findings.data ?? [];

  return (
    <>
      <div className="cluster" style={{ marginBottom: 10 }}>
        {(
          [
            ["open", "مفتوحة"],
            ["acknowledged", "مأخوذة"],
            ["resolved", "محسومة"],
            ["", "الكل"],
          ] as const
        ).map(([value, label]) => (
          <button
            key={value}
            type="button"
            className={`btn btn--sm${state === value ? " btn--primary" : ""}`}
            onClick={() => setState(value)}
          >
            {label}
          </button>
        ))}
      </div>

      {findings.isLoading && <Skeleton height={140} />}

      {findings.isSuccess && rows.length === 0 && (
        <Panel>
          <EmptyState
            kind="clean"
            title={state === "open" ? "لا ملاحظات مفتوحة" : "لا ملاحظات في هذه الحالة"}
            detail="الملاحظة تُغلق نفسها حين يتوقف الفحص عن رؤيتها، وتبقى محسومةً بنصّها حين يغلقها إنسان."
          />
        </Panel>
      )}

      <div className="stack">
        {rows.map((finding) => (
          <FindingCard key={finding.id} finding={finding} />
        ))}
      </div>
    </>
  );
}

function FindingCard({ finding }: { finding: ReconciliationFindingView }) {
  const queryClient = useQueryClient();
  const { can } = useSession();
  const [text, setText] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const invalidate = () =>
    void queryClient.invalidateQueries({ queryKey: ["reconciliation", "findings"] });

  const acknowledge = useMutation({
    mutationFn: () =>
      api.post(`/reconciliation/findings/${finding.id}/acknowledge`, { reason: text.trim() }),
    onSuccess: () => {
      setText("");
      invalidate();
    },
    onError: (e) => isRefusal(e) && setRefusal(e),
  });

  const resolve = useMutation({
    mutationFn: () =>
      api.post(`/reconciliation/findings/${finding.id}/resolve`, { resolution: text.trim() }),
    onSuccess: () => {
      setText("");
      invalidate();
    },
    onError: (e) => isRefusal(e) && setRefusal(e),
  });

  const critical = finding.severity === "critical";
  const open = finding.state === "open" || finding.state === "acknowledged";

  return (
    <Panel
      title={
        <>
          <span className="k ltr">{finding.kind}</span>{" "}
          {critical ? (
            <Chip tone="void" hint="بلغت رؤيتها الثالثة دون أن يأخذها أحد">
              حرجة
            </Chip>
          ) : (
            <Chip tone="pending">{finding.severity}</Chip>
          )}
          <FindingState state={finding.state} />
        </>
      }
      aside={
        <span className="label">
          رُصدت <b className="num">{finding.seen_count}</b> مرة · آخرها{" "}
          {elapsed(finding.last_seen_at)} خلت
        </span>
      }
    >
      <div className="row">
        <span className="label grow">الموضوع</span>
        <span>
          {subjectLabel(finding.subject_type)}{" "}
          <span className="k ltr" title={finding.subject_id}>
            {finding.subject_id.slice(0, 8)}…
          </span>
        </span>
        {finding.subject_type === "account" && (
          <Link className="btn btn--sm" to={`/accounts/${finding.subject_id}`}>
            فتح الحساب
          </Link>
        )}
      </div>

      {Object.keys(finding.detail ?? {}).length > 0 && (
        <dl
          className="note"
          style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "2px 10px", margin: "8px 0" }}
        >
          {Object.entries(finding.detail).map(([key, value]) => (
            <div key={key} style={{ display: "contents" }}>
              <dt style={{ fontFamily: "var(--mono)" }}>{key}</dt>
              <dd style={{ margin: 0 }}>{String(value)}</dd>
            </div>
          ))}
        </dl>
      )}

      {finding.acknowledged_reason && (
        <p className="note">
          أُخذت: «{finding.acknowledged_reason}» — {formatDateTime(finding.acknowledged_at ?? null)}
        </p>
      )}
      {finding.resolution && (
        <p className="note">
          حُسمت: «{finding.resolution}» — {formatDateTime(finding.resolved_at ?? null)}
        </p>
      )}

      {refusal && (
        <div style={{ marginBottom: 8 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      {open && (
        <div className="cluster" style={{ marginTop: 8 }}>
          <input
            className="input"
            style={{ maxWidth: 380 }}
            placeholder={
              finding.state === "open" ? "سبب الأخذ / نص الحسم…" : "نص الحسم (إلزامي)…"
            }
            value={text}
            onChange={(e) => setText(e.target.value)}
          />
          {finding.state === "open" && (
            <Button
              size="sm"
              disabled={!can("account.adjust") || text.trim().length < 3}
              disabledReason={
                !can("account.adjust")
                  ? "الأخذ والحسم عمل مالي — المدقق يراقب ولا يعمل"
                  : "اكتب سبباً"
              }
              busy={acknowledge.isPending}
              onClick={() => acknowledge.mutate()}
            >
              أخذها باسمي
            </Button>
          )}
          <Button
            size="sm"
            variant="primary"
            disabled={!can("account.adjust") || text.trim().length < 3}
            disabledReason={
              !can("account.adjust")
                ? "الأخذ والحسم عمل مالي — المدقق يراقب ولا يعمل"
                : "الحسم يتطلب نصاً يبقى في السجل"
            }
            busy={resolve.isPending}
            onClick={() => resolve.mutate()}
          >
            حسم بنص
          </Button>
        </div>
      )}
    </Panel>
  );
}

function subjectLabel(type: string): string {
  switch (type) {
    case "account":
      return "حساب مالي";
    case "installment":
      return "قسط";
    case "payment":
      return "دفعة";
    case "refund":
      return "استرجاع";
    case "audit":
    case "audit_chain":
      return "سلسلة التدقيق";
    default:
      return type;
  }
}

function FindingState({ state }: { state: string }) {
  switch (state) {
    case "open":
      return <Chip tone="void">مفتوحة</Chip>;
    case "acknowledged":
      return <Chip tone="pending">مأخوذة</Chip>;
    case "resolved":
      return <Chip tone="live">محسومة</Chip>;
    case "auto_closed":
      return <Chip tone="muted" hint="فحص لاحق لم يعد يراها">أُغلقت آلياً</Chip>;
    default:
      return <Chip tone="muted">{state}</Chip>;
  }
}

/* --------------------------------------------------------------------- runs */

function RunsTab() {
  const runs = useQuery({
    queryKey: ["reconciliation", "runs"],
    queryFn: () => api.get<ReconciliationRunView[]>("/reconciliation/runs", { query: { limit: 50 } }),
  });

  const rows = runs.data ?? [];

  if (runs.isLoading) return <Skeleton height={140} />;

  return (
    <>
      <p className="note" style={{ marginBottom: 10, maxWidth: "72ch" }}>
        كل فحص يُسجَّل وجد شيئاً أم لم يجد — الفحص الذي توقّف عن العمل يبدو تماماً كنظام لا خلل
        فيه، وهذا الجدول هو ما يفرّق بينهما.
      </p>
      <Panel title="الفحوصات المسجّلة" flush>
        {rows.length === 0 ? (
          <EmptyState
            kind="not-yet"
            title="لا فحوصات مسجّلة بعد"
            detail="شغّل الفحوصات الآن، أو انتظر تشغيل المجدول الليلي."
          />
        ) : (
          <table className="grid">
            <thead>
              <tr>
                <th>النوع</th>
                <th>بدأ</th>
                <th>الحالة</th>
                <th className="n">صفوف فُحصت</th>
                <th className="n">ملاحظات</th>
                <th className="n">جديدة</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((run) => (
                <tr key={run.id}>
                  <td className="k ltr">{run.kind}</td>
                  <td className="num">{formatDateTime(run.started_at)}</td>
                  <td>
                    {run.status === "completed" || run.status === "clean" ? (
                      run.findings > 0 ? (
                        <Chip tone="pending">وجد شيئاً</Chip>
                      ) : (
                        <Chip tone="live">نظيف</Chip>
                      )
                    ) : run.error ? (
                      <Chip tone="void" hint={run.error}>
                        فشل
                      </Chip>
                    ) : (
                      <Chip tone="muted">{run.status}</Chip>
                    )}
                  </td>
                  <td className="n">{run.rows_checked}</td>
                  <td className="n">{run.findings}</td>
                  <td className="n">{run.new_findings}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </Panel>
    </>
  );
}
