import { useMemo, useState } from "react";
import { Link } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { departmentsOf, useColleges, useDepartments, useStudyTypes } from "@/api/reference";
import type { AccountPlanRow, GenerateAccountsBulkResult } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { amount, sum, type Amount } from "@/lib/money";

/**
 * Bulk account generation — §07 pattern five, and §01's worked example.
 *
 * The failure this screen exists to prevent is named in the specification:
 * pricing 412 students in one click with no preview freezes a wrong price onto
 * 412 accounts, and undoing it is 412 adjustment entries.
 *
 * So: preview first, always. The commit sends back **each row's hash of the
 * outcome that was shown**, and any row whose numbers moved since is skipped
 * with `changed_since_preview` rather than charged under an approval given for
 * different figures. Without that comparison the review is theatre — a fee
 * policy published between the preview and the commit would price hundreds of
 * students under an approval nobody actually gave.
 *
 * The winning policy is displayed and never chosen. Resolution is the source
 * of truth; the reviewer's job is to notice when it picked the wrong row.
 */
export function BulkAccountsScreen() {
  const { can, reason } = useSession();
  const { activeYear } = useWorkingContext();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [studyType, setStudyType] = useState("");
  const [stage, setStage] = useState("");
  const [preview, setPreview] = useState<GenerateAccountsBulkResult | null>(null);
  const [committed, setCommitted] = useState<GenerateAccountsBulkResult | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const scope = () => ({
    academic_year_id: activeYear?.id,
    ...(college ? { college_id: college } : {}),
    ...(department ? { department_id: department } : {}),
    ...(studyType ? { study_type_id: studyType } : {}),
    ...(stage ? { stage: Number(stage) } : {}),
  });

  const run = useMutation({
    mutationFn: (dryRun: boolean) =>
      api.post<GenerateAccountsBulkResult>("/bulk/accounts", {
        ...scope(),
        dry_run: dryRun,
        ...(dryRun
          ? {}
          : {
              // Every approved row carries the hash of what was shown.
              approved: (preview?.rows ?? [])
                .filter((r) => r.preview_hash && r.outcome === "will_create")
                .map((r) => ({ enrollment_id: r.enrollment_id, preview_hash: r.preview_hash })),
            }),
      }),
    onSuccess: (result) => {
      setRefusal(null);
      if (result.dry_run) {
        setPreview(result);
        setCommitted(null);
      } else {
        setCommitted(result);
        setPreview(null);
      }
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const willCreate = preview?.rows.filter((r) => r.outcome === "will_create") ?? [];
  const frozenTotal: Amount = useMemo(
    () => sum(willCreate.map((r) => amount(r.net_total))),
    [willCreate],
  );

  return (
    <main className="screen screen--wide">
      <div className="screen__head">
        <h1 className="screen__title">توليد حسابات جماعي</h1>
        {activeYear && <span className="num">{activeYear.code}</span>}
        {preview && <Chip tone="pending">معاينة — لم يُكتب شيء</Chip>}
      </div>

      <Panel title="النطاق">
        <div className="cols cols--thirds">
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
          <label className="field">
            <span className="field__label">نوع الدراسة</span>
            <select
              className="input"
              value={studyType}
              onChange={(e) => setStudyType(e.target.value)}
            >
              <option value="">كل الأنواع</option>
              {(studyTypes.data ?? []).map((s) => (
                <option key={s.id} value={s.id}>
                  {s.name_ar}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">المرحلة</span>
            <select className="input" value={stage} onChange={(e) => setStage(e.target.value)}>
              <option value="">كل المراحل</option>
              {[1, 2, 3, 4, 5, 6, 7, 8].map((s) => (
                <option key={s} value={s}>
                  {s}
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
          disabled={!can("account.generate") || !activeYear}
          disabledReason={
            !can("account.generate") ? reason("account.generate") : !activeYear ? "اختر السنة" : undefined
          }
          busy={run.isPending && run.variables === true}
          onClick={() => run.mutate(true)}
        >
          معاينة
        </Button>
      </Panel>

      {committed && <CommitReport result={committed} />}

      {preview && (
        <div style={{ marginTop: 12 }}>
          <div className="deck" style={{ marginBottom: 12 }}>
            <Stat value={preview.counts.will_create} label="سيُنشأ" tone="live" />
            <Stat
              value={preview.counts.already_has_account}
              label="له حساب أصلاً"
              tone="muted"
            />
            <Stat value={preview.counts.blocked} label="ممنوع — بسبب لكل صف" tone="void" />
            <Stat
              value={<Money value={frozenTotal} tone="plain" />}
              label="الالتزام الذي سيُجمَّد"
            />
          </div>

          <Panel title="المعاينة صفاً بصف" flush>
            <PlanTable rows={preview.rows} />
          </Panel>

          {preview.skip_reasons && Object.keys(preview.skip_reasons).length > 0 && (
            <Panel title="أسباب التخطّي" >
              {/* Grouped by code, not listed line by line: eighty identical
                  failures are one fact, not eighty sentences. */}
              {Object.entries(preview.skip_reasons).map(([code, count]) => (
                <div className="row" key={code}>
                  <span className="k grow">{code}</span>
                  <span className="num">{count}</span>
                </div>
              ))}
            </Panel>
          )}

          <div className="cluster" style={{ marginTop: 12 }}>
            <Button
              variant="primary"
              size="lg"
              disabled={!can("account.generate") || willCreate.length === 0}
              disabledReason={
                willCreate.length === 0 ? "لا صفوف قابلة للإنشاء في هذه المعاينة" : undefined
              }
              busy={run.isPending && run.variables === false}
              onClick={() => run.mutate(false)}
            >
              اعتماد {willCreate.length} صفاً وتنفيذها
            </Button>
            <Button variant="ghost" onClick={() => setPreview(null)}>
              إلغاء المعاينة
            </Button>
            <span className="grow" />
            <span className="note" style={{ maxWidth: "42ch" }}>
              التنفيذ يرسل بصمة كل صف؛ أي صف تغيّرت أرقامه منذ المعاينة يُتخطّى ويُدرَج في تقرير
              الختام بسبب <span className="ltr k">changed_since_preview</span>.
            </span>
          </div>
        </div>
      )}
    </main>
  );
}

function CommitReport({ result }: { result: GenerateAccountsBulkResult }) {
  return (
    <div style={{ marginTop: 12 }}>
      <Panel
        title="تقرير الختام"
        aside={
          result.counts.skipped > 0 || result.counts.failed > 0 ? (
            <Chip tone="pending">مكتمل مع تخطّيات</Chip>
          ) : (
            <Chip tone="live">مكتمل</Chip>
          )
        }
      >
        <div className="deck">
          <Stat value={result.counts.created} label="أُنشئ" tone="live" />
          <Stat value={result.counts.skipped} label="تُخطّي" tone="pending" />
          <Stat value={result.counts.failed} label="فشل" tone="void" />
          <Stat value={result.counts.total} label="الإجمالي" />
        </div>
        {result.skip_reasons && Object.keys(result.skip_reasons).length > 0 && (
          <div style={{ marginTop: 12 }}>
            <span className="field__label">أسباب التخطّي، مجمّعة بالرمز</span>
            {Object.entries(result.skip_reasons).map(([code, count]) => (
              <div className="row" key={code}>
                <span className="k grow">{code}</span>
                <span className="num">{count}</span>
              </div>
            ))}
          </div>
        )}
        <p className="note" style={{ marginTop: 10 }}>
          الحالة «مكتمل مع تخطّيات» لا «تم»: صفٌّ مسموم لا يُسقط الباقين، وكل صف التزم على حدة.
        </p>
      </Panel>
    </div>
  );
}

function PlanTable({ rows }: { rows: AccountPlanRow[] }) {
  if (rows.length === 0) {
    return <EmptyState kind="no-results" title="لا صفوف في هذا النطاق" />;
  }
  return (
    <div className="table-wrap" style={{ border: 0, maxHeight: "60vh", overflowY: "auto" }}>
      <table className="grid">
        <thead>
          <tr>
            <th>الطالب</th>
            <th className="n">المرحلة</th>
            <th>السياسة الفائزة</th>
            <th className="n col-group-money">الإجمالي</th>
            <th className="n">الخصم</th>
            <th className="n">الصافي</th>
            <th>الحالة</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.enrollment_id}>
              <td>
                <Link to={`/students/${row.student_id}`}>فتح ملف الطالب</Link>
              </td>
              <td className="n">{row.stage}</td>
              <td>
                {row.fee_policy_code ? (
                  <>
                    <span className="ltr k">{row.fee_policy_code}</span>{" "}
                    <span className="label">({row.fee_policy_specificity})</span>
                  </>
                ) : (
                  <span className="label">لا توجد سياسة مطابقة</span>
                )}
              </td>
              <td className="n col-group-money">
                {row.outcome === "will_create" || row.outcome === "created" ? (
                  <Money value={row.gross_total} tone="plain" />
                ) : (
                  <span className="money money--absent">—</span>
                )}
              </td>
              <td className="n">
                {row.outcome === "will_create" || row.outcome === "created" ? (
                  <Money value={-amount(row.discount_total)} />
                ) : (
                  <span className="money money--absent">—</span>
                )}
              </td>
              <td className="n">
                {row.outcome === "will_create" || row.outcome === "created" ? (
                  <Money value={row.net_total} tone="plain" />
                ) : (
                  <span className="money money--absent">—</span>
                )}
              </td>
              <td>
                <OutcomeChip row={row} />
              </td>
            </tr>
          ))}
        </tbody>
      </table>
      <p className="note" style={{ padding: "8px 12px" }}>
        السياسة <b>تُعرض ولا تُختار</b> — المصدر هو خوارزمية الترجيح، ومهمة المراجع أن يلاحظ حين
        تفوز السياسة الخطأ. الرقم بين قوسين هو درجة التخصيص.
      </p>
    </div>
  );
}

function OutcomeChip({ row }: { row: AccountPlanRow }) {
  switch (row.outcome) {
    case "will_create":
      return <Chip tone="live">سيُنشأ</Chip>;
    case "created":
      return <Chip tone="live">أُنشئ</Chip>;
    case "already_has_account":
      return <Chip tone="muted">له حساب أصلاً</Chip>;
    case "blocked":
      return (
        <Chip tone="void" hint={row.reason || row.error_code}>
          ممنوع
        </Chip>
      );
    case "skipped":
      return (
        <Chip tone="pending" hint={row.reason || row.error_code}>
          تُخطّي
        </Chip>
      );
    default:
      return (
        <Chip tone="muted" hint={row.reason || row.error_code}>
          {row.outcome}
        </Chip>
      );
  }
}
