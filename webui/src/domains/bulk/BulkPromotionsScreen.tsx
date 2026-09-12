import { useEffect, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { departmentsOf, useColleges, useDepartments } from "@/api/reference";
import type { PromoteBulkResult } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

/**
 * Bulk promotion — §09.
 *
 * Preview → plan hash → commit. The commit must present the hash the dry run
 * returned, and the server re-derives the plan before applying anything, so a
 * result nobody approved cannot slip in between.
 *
 * Two rules are visible on the screen because they are the ones that surprise
 * people: a pass at the department's **final stage completes** the enrollment
 * rather than promoting into a stage that does not exist, and a failure
 * re-registers at the same stage as a repeat — which is usually priced by a
 * different policy, and is derived from recorded results rather than typed.
 */
export function BulkPromotionsScreen() {
  const { can, reason } = useSession();
  const { years, activeYear } = useWorkingContext();
  const colleges = useColleges();
  const departments = useDepartments();

  const [sourceYear, setSourceYear] = useState("");
  const [targetYear, setTargetYear] = useState("");
  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [plan, setPlan] = useState<PromoteBulkResult | null>(null);
  const [report, setReport] = useState<PromoteBulkResult | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  useEffect(() => {
    if (!targetYear && activeYear) setTargetYear(activeYear.id);
  }, [activeYear, targetYear]);

  const run = useMutation({
    mutationFn: (dryRun: boolean) =>
      api.post<PromoteBulkResult>("/bulk/promotions", {
        source_year_id: sourceYear,
        target_year_id: targetYear,
        ...(college ? { college_id: college } : {}),
        ...(department ? { department_id: department } : {}),
        dry_run: dryRun,
        // The commit presents the plan's fingerprint back; the server
        // re-derives it and refuses a plan that moved.
        ...(dryRun ? {} : { approve_plan_hash: plan?.plan_hash }),
      }),
    onSuccess: (result) => {
      setRefusal(null);
      if (result.dry_run) {
        setPlan(result);
        setReport(null);
      } else {
        setReport(result);
        setPlan(null);
      }
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready = Boolean(sourceYear && targetYear && sourceYear !== targetYear);

  return (
    <main className="screen screen--wide">
      <div className="screen__head">
        <h1 className="screen__title">الترقية الجماعية</h1>
        {plan && <Chip tone="pending">خطة — لم يُكتب شيء</Chip>}
      </div>

      <Panel title="النطاق">
        <div className="cols cols--thirds">
          <label className="field">
            <span className="field__label">من سنة</span>
            <select
              className="input"
              value={sourceYear}
              onChange={(e) => setSourceYear(e.target.value)}
            >
              <option value="">— اختر —</option>
              {years.map((y) => (
                <option key={y.id} value={y.id}>
                  {y.code}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">إلى سنة</span>
            <select
              className="input"
              value={targetYear}
              onChange={(e) => setTargetYear(e.target.value)}
            >
              <option value="">— اختر —</option>
              {years.map((y) => (
                <option key={y.id} value={y.id}>
                  {y.code}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">الكلية</span>
            <select
              className="input"
              value={college}
              onChange={(e) => {
                setCollege(e.target.value);
                setDepartment("");
              }}
            >
              <option value="">كل الكليات</option>
              {(colleges.data ?? []).map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name_ar}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">القسم</span>
            <select
              className="input"
              value={department}
              onChange={(e) => setDepartment(e.target.value)}
              disabled={!college}
            >
              <option value="">كل الأقسام</option>
              {departmentsOf(departments.data, college).map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name_ar}
                </option>
              ))}
            </select>
          </label>
        </div>

        {refusal && (
          <div style={{ marginBottom: 10 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}

        <Button
          variant="primary"
          disabled={!can("enrollment.result") || !ready}
          disabledReason={
            !can("enrollment.result")
              ? reason("enrollment.result")
              : !ready
                ? "اختر سنتين مختلفتين"
                : undefined
          }
          busy={run.isPending && run.variables === true}
          onClick={() => run.mutate(true)}
        >
          معاينة الخطة
        </Button>
      </Panel>

      <div className="callout callout--note" style={{ marginTop: 12 }}>
        النجاح في المرحلة الأخيرة للقسم <b>يُكمل</b> التسجيل ولا يُرقّيه — لا توجد مرحلة سابعة في
        برنامج من ستّ. والرسوب يُعيد التسجيل على المرحلة نفسها بفئة «معيد»، وهي الحقل الذي تُسعِّر
        عليه سياسة المعيدين، ويُشتق من النتائج المسجَّلة لا من كتابة موظف.
      </div>

      {report && <PromotionReport result={report} />}

      {plan && (
        <div style={{ marginTop: 12 }}>
          <div className="deck" style={{ marginBottom: 12 }}>
            <Stat value={plan.counts.promoted} label="سيُرقّى" tone="live" />
            <Stat value={plan.counts.repeated} label="سيُعيد" tone="pending" />
            <Stat value={plan.counts.completed} label="سيُكمل" tone="frozen" />
            <Stat value={plan.counts.skipped} label="سيُتخطّى" tone="muted" />
            <Stat value={plan.counts.total} label="الإجمالي" />
          </div>

          {plan.skip_reasons && Object.keys(plan.skip_reasons).length > 0 && (
            <Panel title="أسباب التخطّي، مجمّعة بالرمز">
              {Object.entries(plan.skip_reasons).map(([code, count]) => (
                <div className="row" key={code}>
                  <span className="k grow">{code}</span>
                  <span className="num">{count}</span>
                </div>
              ))}
            </Panel>
          )}

          <Panel title="الخطة صفاً بصف" flush>
            {plan.rows.length === 0 ? (
              <EmptyState kind="no-results" title="لا صفوف في هذا النطاق" />
            ) : (
              <div className="table-wrap" style={{ border: 0, maxHeight: "50vh", overflowY: "auto" }}>
                <table className="grid">
                  <thead>
                    <tr>
                      <th>الطالب</th>
                      <th className="n">من مرحلة</th>
                      <th className="n">إلى مرحلة</th>
                      <th>النتيجة</th>
                      <th>السبب</th>
                    </tr>
                  </thead>
                  <tbody>
                    {plan.rows.map((row) => (
                      <tr key={row.enrollment_id}>
                        <td>
                          {row.full_name ?? row.student_no ?? (
                            <Link to={`/students/${row.student_id}`}>فتح ملف الطالب</Link>
                          )}
                        </td>
                        <td className="n">{row.from_stage ?? "—"}</td>
                        <td className="n">{row.to_stage ?? "—"}</td>
                        <td>
                          <PromotionOutcome outcome={row.outcome} />
                        </td>
                        <td className="label">{row.reason ?? row.error_code ?? "—"}</td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              </div>
            )}
          </Panel>

          <div className="cluster" style={{ marginTop: 12 }}>
            <Button
              variant="primary"
              size="lg"
              busy={run.isPending && run.variables === false}
              onClick={() => run.mutate(false)}
            >
              اعتماد الخطة وتنفيذها
            </Button>
            <Button variant="ghost" onClick={() => setPlan(null)}>
              إلغاء
            </Button>
            <span className="grow" />
            <span className="note ltr" title="بصمة الخطة">
              plan_hash {plan.plan_hash.slice(0, 12)}…
            </span>
          </div>
        </div>
      )}
    </main>
  );
}

function PromotionOutcome({ outcome }: { outcome: string }) {
  switch (outcome) {
    case "promoted":
      return <Chip tone="live">ترقية</Chip>;
    case "repeated":
      return <Chip tone="pending">إعادة</Chip>;
    case "completed":
      return <Chip tone="frozen">إكمال</Chip>;
    case "skipped":
      return <Chip tone="muted">تخطٍّ</Chip>;
    case "failed":
      return <Chip tone="void">فشل</Chip>;
    default:
      return <Chip tone="muted">{outcome}</Chip>;
  }
}

function PromotionReport({ result }: { result: PromoteBulkResult }) {
  const withSkips = result.counts.skipped > 0 || result.counts.failed > 0;
  return (
    <div style={{ marginTop: 12 }}>
      <Panel
        title="تقرير الختام"
        aside={
          withSkips ? <Chip tone="pending">مكتمل مع تخطّيات</Chip> : <Chip tone="live">مكتمل</Chip>
        }
      >
        <div className="deck">
          <Stat value={result.counts.promoted} label="رُقّي" tone="live" />
          <Stat value={result.counts.repeated} label="أعاد" tone="pending" />
          <Stat value={result.counts.completed} label="أكمل" tone="frozen" />
          <Stat value={result.counts.skipped} label="تُخطّي" tone="muted" />
          <Stat value={result.counts.failed} label="فشل" tone="void" />
        </div>
        {result.skip_reasons && Object.keys(result.skip_reasons).length > 0 && (
          <div style={{ marginTop: 12 }}>
            <span className="field__label">التخطّيات، مجمّعة بالرمز لا مسرودة</span>
            {Object.entries(result.skip_reasons).map(([code, count]) => (
              <div className="row" key={code}>
                <span className="k grow">{code}</span>
                <span className="num">{count}</span>
              </div>
            ))}
          </div>
        )}
      </Panel>
    </div>
  );
}
