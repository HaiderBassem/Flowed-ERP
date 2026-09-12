import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import {
  departmentsOf,
  STUDENT_CATEGORIES,
  useColleges,
  useDepartments,
  useStudyTypes,
} from "@/api/reference";
import type { FeePolicyView, FeeResolutionPreviewView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

/**
 * Fee policies, and the resolution tester — §09 calls this the hardest screen
 * in the system, and names why: six dimensions and a wildcard in each.
 *
 * The answer it prescribes is the tester. An operator enters a concrete case
 * — year, college, department, stage, study type, category — and sees which
 * policy would price it, what its specificity score is, **and why each rival
 * lost**. Without that, a wrong policy is not discovered until it has been
 * frozen onto hundreds of accounts, and unfreezing it is hundreds of
 * adjustment entries.
 *
 * The published policy has no edit control. A published policy priced real
 * accounts, and those accounts point at this exact version; changing it would
 * rewrite history that receipts already reference. Its place is a new version.
 */
export function FeePoliciesScreen() {
  const { activeYear } = useWorkingContext();
  const { can, reason } = useSession();

  const policies = useQuery({
    queryKey: ["fee-policies", activeYear?.id],
    queryFn: () =>
      api.get<FeePolicyView[]>("/fee-policies", { query: { academic_year_id: activeYear?.id } }),
    enabled: Boolean(activeYear),
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">سياسات الرسوم</h1>
        {activeYear && <span className="num">{activeYear.code}</span>}
      </div>

      <div className="cols" style={{ gridTemplateColumns: "minmax(0,1.4fr) minmax(340px,1fr)", marginTop: 12 }}>
        <div className="stack">
          <Panel
            title="السياسات المعرّفة"
            aside={<span className="label">الترجيح بالتخصيص — الأعلى درجةً يفوز</span>}
            flush
          >
            {policies.isLoading && <Skeleton height={160} />}
            {policies.isError &&
              (isRefusal(policies.error) ? (
                <RefusalPanel refusal={policies.error} onRetry={() => void policies.refetch()} />
              ) : (
                <EmptyState kind="no-results" title="تعذّر جلب السياسات" />
              ))}
            {policies.isSuccess && (policies.data ?? []).length === 0 && (
              <EmptyState
                kind="not-yet"
                title="لا سياسات لهذه السنة"
                detail="بلا سياسة منشورة لا يمكن توليد أي حساب — التسعير يبدأ من هنا."
              />
            )}
            {(policies.data ?? []).length > 0 && (
              <table className="grid">
                <thead>
                  <tr>
                    <th>الرمز</th>
                    <th className="n">الإصدار</th>
                    <th>النطاق</th>
                    <th className="n">درجة التخصيص</th>
                    <th className="n col-group-money">الإجمالي</th>
                    <th className="n">سقف الخصم</th>
                    <th>الحالة</th>
                  </tr>
                </thead>
                <tbody>
                  {(policies.data ?? []).map((policy) => (
                    <tr key={policy.id}>
                      <td className="k">{policy.policy_code}</td>
                      <td className="n">{policy.version_no}</td>
                      <td>
                        <ScopeSummary policy={policy} />
                      </td>
                      <td className="n">{policy.specificity_score}</td>
                      <td className="n col-group-money">
                        <Money value={policy.gross_total} tone="plain" />
                      </td>
                      <td className="n">{(policy.max_discount_bp / 100).toFixed(0)}٪</td>
                      <td>
                        <PolicyStatus status={policy.status} />
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="القاعدة">
            <p className="note" style={{ margin: 0 }}>
              السياسة المنشورة <b>لا تُعدَّل</b>، ولذلك لا يوجد زر تعديل هنا: هذه السياسة سعّرت
              حساباتٍ فعلية، وتلك الحسابات تشير إلى هذا الإصدار بالذات. تغييرها يعيد كتابة تاريخ
              تحمله وصولات مطبوعة. البديل إصدار جديد، والقديم يبقى مربوطاً بما سعّره.
            </p>
          </Panel>
        </div>

        <ResolutionTester
          canWrite={can("config.write")}
          writeReason={reason("config.write")}
        />
      </div>
    </main>
  );
}

function PolicyStatus({ status }: { status: string }) {
  if (status === "published") return <Chip tone="live">منشورة</Chip>;
  if (status === "retired") return <Chip tone="muted">متقاعدة</Chip>;
  return (
    <Chip tone="pending" hint="المسودة لا تسعّر شيئاً حتى تُنشر">
      مسودة
    </Chip>
  );
}

/** The six dimensions, with a wildcard shown as an explicit "any". */
function ScopeSummary({ policy }: { policy: FeePolicyView }) {
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const parts: string[] = [];
  parts.push(
    policy.college_id
      ? (colleges.data?.find((c) => c.id === policy.college_id)?.name_ar ?? "كلية")
      : "أي كلية",
  );
  parts.push(
    policy.department_id
      ? (departments.data?.find((d) => d.id === policy.department_id)?.name_ar ?? "قسم")
      : "أي قسم",
  );
  parts.push(policy.stage ? `المرحلة ${policy.stage}` : "أي مرحلة");
  parts.push(
    policy.study_type_id
      ? (studyTypes.data?.find((s) => s.id === policy.study_type_id)?.name_ar ?? "نوع")
      : "أي نوع",
  );

  return <span className="label">{parts.join(" · ")}</span>;
}

/**
 * The resolution tester.
 *
 * Reads only — it writes nothing and prices nothing. Its whole value is
 * letting somebody notice that the wrong policy wins **before** a generation
 * freezes it onto a cohort.
 */
function ResolutionTester({
  canWrite,
  writeReason,
}: {
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  const { activeYear } = useWorkingContext();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();
  const queryClient = useQueryClient();

  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [stage, setStage] = useState("1");
  const [studyType, setStudyType] = useState("");
  const [category, setCategory] = useState("REGULAR");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [result, setResult] = useState<FeeResolutionPreviewView | null>(null);

  const departmentOptions = departmentsOf(departments.data, college);

  const preview = useMutation({
    mutationFn: () =>
      api.post<FeeResolutionPreviewView>("/fee-policies/preview-resolution", {
        academic_year_id: activeYear!.id,
        college_id: college,
        department_id: department,
        stage: Number(stage),
        study_type_id: studyType,
        student_category_code: category,
      }),
    onSuccess: (data) => {
      setResult(data);
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["fee-policies"] });
    },
    onError: (error) => {
      setResult(null);
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready = Boolean(activeYear && college && department && studyType);

  return (
    <div className="stack">
      <Panel title="مُجرِّب الترجيح" aside={<span className="label">قراءة فقط — لا يكتب شيئاً</span>}>
        <p className="note" style={{ marginBottom: 10 }}>
          أدخل حالة طالب حقيقية، وسترى أي سياسة تسعّرها ولماذا خسر كل منافس.
        </p>

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
            <option value="">— اختر —</option>
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
            <option value="">— اختر —</option>
            {departmentOptions.map((d) => (
              <option key={d.id} value={d.id}>
                {d.name_ar}
              </option>
            ))}
          </select>
        </label>

        <div className="cols cols--half">
          <label className="field">
            <span className="field__label">المرحلة</span>
            <select className="input" value={stage} onChange={(e) => setStage(e.target.value)}>
              {[1, 2, 3, 4, 5, 6, 7, 8].map((s) => (
                <option key={s} value={s}>
                  {s}
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
              <option value="">— اختر —</option>
              {(studyTypes.data ?? []).map((s) => (
                  <option key={s.id} value={s.id}>
                    {s.name_ar}
                  </option>
                ))}
            </select>
          </label>
        </div>

        <label className="field">
          <span className="field__label">فئة الطالب</span>
          <select className="input" value={category} onChange={(e) => setCategory(e.target.value)}>
            {STUDENT_CATEGORIES.map((c) => (
              <option key={c.code} value={c.code}>
                {c.name}
              </option>
            ))}
          </select>
          <span className="field__hint">
            «معيد» عادةً يُسعَّر بسياسة مختلفة — وهي أكثر حقل يُنسى في المراجعة.
          </span>
        </label>

        <Button
          variant="primary"
          disabled={!ready}
          disabledReason={!activeYear ? "اختر السنة أولاً" : !ready ? "أكمل الحقول" : undefined}
          busy={preview.isPending}
          onClick={() => preview.mutate()}
        >
          جرّب الترجيح
        </Button>
      </Panel>

      {refusal && <RefusalPanel refusal={refusal} />}

      {result && (
        <Panel title="النتيجة">
          {!result.resolvable || !result.winner ? (
            <>
              <div className="refusal">
                <p className="refusal__title">لا سياسة تطابق هذه الحالة</p>
                <p className="refusal__body" style={{ margin: 0 }}>
                  {result.explanation ||
                    "لا توجد سياسة منشورة تغطي هذا النطاق — أي توليد حساب لهذه الحالة سيُرفض."}
                </p>
              </div>
              <p className="note" style={{ marginTop: 10 }}>
                هذا بالضبط ما يظهر في معاينة التوليد الجماعي بحالة «ممنوع».
              </p>
            </>
          ) : (
            <>
              <Row label="السياسة الفائزة">
                <span className="k ltr">
                  {result.winner.policy_code} v{result.winner.version_no}
                </span>
              </Row>
              <Row label="درجة التخصيص">
                <span className="num">{result.winner.specificity_score}</span>
              </Row>
              <Row label="الإجمالي">
                <Money value={result.winner.gross_total} size="big" tone="plain" />
              </Row>
              <Row label="الأبعاد المحدَّدة">
                <span className="label">{result.winner.dimensions.join(" · ") || "لا شيء"}</span>
              </Row>

              {result.explanation && (
                <p className="note" style={{ marginTop: 8 }}>
                  {result.explanation}
                </p>
              )}

              {/* Why each rival lost — the part that makes this a tester
                  rather than a lookup. */}
              <div style={{ marginTop: 14 }}>
                <span className="field__label">المنافسون الخاسرون</span>
                {result.runners_up.length === 0 ? (
                  <p className="note">لا منافس — سياسة واحدة تغطي هذا النطاق.</p>
                ) : (
                  <table className="grid" style={{ marginTop: 6 }}>
                    <thead>
                      <tr>
                        <th>السياسة</th>
                        <th className="n">التخصيص</th>
                        <th className="n col-group-money">الإجمالي</th>
                        <th>سبب الخسارة</th>
                      </tr>
                    </thead>
                    <tbody>
                      {result.runners_up.map((candidate) => (
                        <tr key={candidate.policy_id}>
                          <td className="k ltr">
                            {candidate.policy_code} v{candidate.version_no}
                          </td>
                          <td className="n">{candidate.specificity_score}</td>
                          <td className="n col-group-money">
                            <Money value={candidate.gross_total} tone="plain" />
                          </td>
                          <td className="label">
                            أقل تخصيصاً بـ{" "}
                            <span className="num">
                              {result.winner!.specificity_score - candidate.specificity_score}
                            </span>{" "}
                            درجة
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                )}
              </div>
            </>
          )}
        </Panel>
      )}

      {!canWrite && (
        <p className="note">
          التعريف والنشر صلاحية المدير المالي — {writeReason}. المُجرِّب أعلاه متاح للقراءة.
        </p>
      )}
    </div>
  );
}
