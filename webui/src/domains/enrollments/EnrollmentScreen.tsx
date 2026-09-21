import { useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { departmentsOf, useColleges, useDepartments, useStudyTypes } from "@/api/reference";
import type {
  AccountView,
  ChangeStatusResponse,
  EnrollmentView,
  StudentView,
} from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

import { amount } from "@/lib/money";
import { labelEnrollmentKind, labelResult } from "@/design/lexicon";

type Sheet = "supersede" | "result" | "status" | null;

/**
 * One enrollment, and the commands legitimate on it now.
 *
 * The enrollment is the financial unit, so this page is where the academic
 * context and the money meet: its account, its supersede lineage, and the
 * three commands that can change it.
 */
export function EnrollmentScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();
  const { years } = useWorkingContext();
  const [sheet, setSheet] = useState<Sheet>(null);

  const enrollment = useQuery({
    queryKey: ["enrollment", id],
    queryFn: () => api.get<EnrollmentView>(`/enrollments/${id}`),
    enabled: Boolean(id),
  });

  const student = useQuery({
    queryKey: ["student", enrollment.data?.student_id],
    queryFn: () => api.get<StudentView>(`/students/${enrollment.data!.student_id}`),
    enabled: Boolean(enrollment.data?.student_id),
  });

  const accounts = useQuery({
    queryKey: ["student-accounts", enrollment.data?.student_id],
    queryFn: () => api.get<AccountView[]>(`/students/${enrollment.data!.student_id}/accounts`),
    enabled: Boolean(enrollment.data?.student_id),
  });

  if (enrollment.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  if (enrollment.isError) {
    return (
      <main className="screen">
        {isRefusal(enrollment.error) ? (
          <RefusalPanel refusal={enrollment.error} onRetry={() => void enrollment.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح التسجيل" />
        )}
      </main>
    );
  }

  const view = enrollment.data!;
  const year = years.find((y) => y.id === view.academic_year_id);
  const account = (accounts.data ?? []).find((a) => a.enrollment_id === view.id);

  const superseded = view.status === "superseded";
  const yearOpen = year?.accepts_academic_recording ?? false;

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: "ملف الطالب", to: `/students/${view.student_id}` },
          { label: `تسجيل ${year?.code ?? ""}` },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">
          التسجيل — <span className="num">{year?.code ?? ""}</span>
        </h1>
        <StateChip entity="enrollment" status={view.status} />
        <span className="grow" />
        <Link className="btn" to={`/students/${view.student_id}`}>
          ملف الطالب
        </Link>
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <Panel title="السياق">
          <Row label="الطالب">
            <Link to={`/students/${view.student_id}`}>{student.data?.full_name ?? "…"}</Link>
          </Row>
          <Row label="الرقم الجامعي">
            <span className="num">{student.data?.student_no ?? "…"}</span>
          </Row>
          <Row label="المرحلة">
            <span className="num">{view.stage}</span>
          </Row>
          <Row label="المحاولة">
            <span className="num">{view.attempt_number}</span>
            {view.is_repeat && (
              <Chip tone="pending" hint="فئة «معيد» تُسعَّر بسياسة مختلفة عادةً">
                معيد
              </Chip>
            )}
          </Row>
          <Row label="النوع">{labelEnrollmentKind(view.kind)}</Row>
          <Row label="النتيجة">
            {labelResult(view.result)}
            {view.result_by_decision && <span className="label"> (بقرار)</span>}
          </Row>
          {view.supersedes_id && (
            <Row label="استبدل">
              <Link className="ltr k" to={`/enrollments/${view.supersedes_id}`}>
                التسجيل السابق
              </Link>
              {view.supersede_reason && (
                <span className="label"> — {view.supersede_reason}</span>
              )}
            </Row>
          )}
        </Panel>

        <Panel title="الحساب المالي">
          {account ? (
            <>
              <Row label="الحالة">
                <StateChip entity="account" status={account.status} />
              </Row>
              <Row label="الصافي الفعّال">
                <Money value={account.effective_net} tone="plain" />
              </Row>
              <Row label="المتبقي">
                <Money value={account.remaining} />
              </Row>
              <Link className="btn btn--sm" to={`/accounts/${account.id}`}>
                فتح الحساب
              </Link>
            </>
          ) : (
            <EmptyState
              kind="not-yet"
              title="لا حساب على هذا التسجيل"
              detail="التسجيل لا يولّد حساباً — التسعير أمر منفصل."
              action={
                can("account.generate") ? (
                  <Link className="btn btn--primary" to={`/accounts/new?enrollment=${view.id}`}>
                    توليد حساب
                  </Link>
                ) : null
              }
            />
          )}
        </Panel>
      </div>

      <div style={{ marginTop: 12 }}>
        <Panel title="الأوامر المشروعة الآن">
          <div className="cluster">
            <Button
              disabled={!can("enrollment.write") || superseded}
              disabledReason={
                !can("enrollment.write")
                  ? reason("enrollment.write")
                  : superseded
                    ? "تسجيل مستبدَل لا يُستبدَل ثانيةً"
                    : undefined
              }
              onClick={() => setSheet("supersede")}
              title="تغيير القسم أو نوع الدراسة أو المرحلة في منتصف السنة"
            >
              استبدال (Supersede)
            </Button>
            <Button
              disabled={!can("enrollment.result") || superseded}
              disabledReason={
                !can("enrollment.result")
                  ? reason("enrollment.result")
                  : superseded
                    ? "تسجيل مستبدَل لا يُسجَّل له نتيجة"
                    : undefined
              }
              onClick={() => setSheet("result")}
            >
              تسجيل نتيجة
            </Button>
            <Button
              disabled={!can("enrollment.write") || superseded}
              disabledReason={!can("enrollment.write") ? reason("enrollment.write") : undefined}
              onClick={() => setSheet("status")}
            >
              تغيير الحالة
            </Button>
            {can("hosting.write") && (
              <Link className="btn" to={`/hosting?enrollment=${view.id}`}>
                اتفاقية استضافة
              </Link>
            )}
          </div>

          {/* Two closes: results keep going after the money freezes. */}
          {year && !year.accepts_financial_posting && yearOpen && (
            <p className="note" style={{ marginTop: 10 }}>
              هذه السنة مغلقة مالياً والتسجيل الأكاديمي فيها ما زال مفتوحاً — إدخال النتائج يعمل.
              هذا بالضبط سبب وجود إغلاقين.
            </p>
          )}
        </Panel>
      </div>

      {sheet === "supersede" && (
        <SupersedeWizard enrollment={view} account={account} onDone={() => setSheet(null)} />
      )}
      {sheet === "result" && <ResultSheet enrollment={view} onDone={() => setSheet(null)} />}
      {sheet === "status" && <StatusSheet enrollment={view} onDone={() => setSheet(null)} />}
    </main>
  );
}

/**
 * The supersede wizard — §09 names the second step as the important one.
 *
 * The financial consequence is laid out in full **before** confirmation: the
 * old account is cancelled, what was paid moves across as two visible transfer
 * entries, and a new account is generated under the new context's policy. The
 * payments themselves never move, and the screen says why — printed receipts
 * point at them.
 */
function SupersedeWizard({
  enrollment,
  account,
  onDone,
}: {
  enrollment: EnrollmentView;
  account: AccountView | undefined;
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const [step, setStep] = useState<1 | 2 | 3>(1);
  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [studyType, setStudyType] = useState("");
  const [stage, setStage] = useState(String(enrollment.stage));
  const [effectiveDate, setEffectiveDate] = useState(new Date().toISOString().slice(0, 10));
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const run = useMutation({
    mutationFn: () =>
      api.post(`/enrollments/${enrollment.id}/supersede`, {
        ...(department ? { new_department_id: department } : {}),
        ...(studyType ? { new_study_type_id: studyType } : {}),
        ...(Number(stage) !== enrollment.stage ? { new_stage: Number(stage) } : {}),
        reason: reason.trim(),
        effective_date: effectiveDate,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["enrollment"] });
      void queryClient.invalidateQueries({ queryKey: ["student-enrollments"] });
      void queryClient.invalidateQueries({ queryKey: ["student-accounts"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const paid = account ? amount(account.net_paid) : 0n;
  const changed = Boolean(department || studyType || Number(stage) !== enrollment.stage);

  return (
    <div style={{ marginTop: 12 }}>
      <Panel
        title="معالج الاستبدال"
        aside={
          <span className="cluster">
            {[1, 2, 3].map((n) => (
              <span
                key={n}
                className="sheet__stage"
                data-state={step === n ? "current" : step > n ? "done" : undefined}
              >
                {n} · {n === 1 ? "السياق الجديد" : n === 2 ? "الأثر المالي" : "التأكيد"}
              </span>
            ))}
          </span>
        }
      >
        {step === 1 && (
          <>
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
                <option value="">— بلا تغيير —</option>
                {(colleges.data ?? []).map((c) => (
                  <option key={c.id} value={c.id}>
                    {c.name_ar}
                  </option>
                ))}
              </select>
            </label>

            <label className="field">
              <span className="field__label">القسم الجديد</span>
              <select
                className="input"
                value={department}
                onChange={(e) => setDepartment(e.target.value)}
                disabled={!college}
              >
                <option value="">— بلا تغيير —</option>
                {departmentsOf(departments.data, college).map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name_ar}
                  </option>
                ))}
              </select>
            </label>

            <div className="cols cols--half">
              <label className="field">
                <span className="field__label">نوع الدراسة الجديد</span>
                <select
                  className="input"
                  value={studyType}
                  onChange={(e) => setStudyType(e.target.value)}
                >
                  <option value="">— بلا تغيير —</option>
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
                  {[1, 2, 3, 4, 5].map((s) => (
                    <option key={s} value={s}>
                      {s}
                    </option>
                  ))}
                </select>
              </label>
            </div>

            <label className="field">
              <span className="field__label">تاريخ النفاذ</span>
              <input
                className="input num"
                type="date"
                value={effectiveDate}
                onChange={(e) => setEffectiveDate(e.target.value)}
              />
            </label>

            <div className="cluster">
              <Button
                variant="primary"
                disabled={!changed}
                disabledReason={!changed ? "غيّر شيئاً واحداً على الأقل" : undefined}
                onClick={() => setStep(2)}
              >
                التالي — الأثر المالي
              </Button>
              <Button variant="ghost" onClick={onDone}>
                صرف النظر
              </Button>
            </div>
          </>
        )}

        {step === 2 && (
          <>
            <p className="note" style={{ marginBottom: 12 }}>
              هذا ما سيحدث للمال. اقرأه قبل التأكيد — بعده لا تصحيح إلا بقيود.
            </p>

            <div className="waterfall">
              <div className="waterfall__line">
                <span className="grow">
                  <b>1.</b> الحساب الحالي يُلغى
                </span>
                {account ? (
                  <Chip tone="void">يُلغى</Chip>
                ) : (
                  <span className="label">لا حساب — لا شيء يُلغى</span>
                )}
              </div>
              <div className="waterfall__line">
                <span className="grow">
                  <b>2.</b> المدفوع ينتقل بقيدَي نقل ظاهرين
                </span>
                <Money value={paid} tone="positive" />
              </div>
              <div className="waterfall__line">
                <span className="grow">
                  <b>3.</b> حساب جديد يُولَّد بسياسة السياق الجديد
                </span>
                <span className="label">بأسعار السياق الجديد</span>
              </div>
              <div className="waterfall__line waterfall__line--effective">
                <span className="grow">
                  <b style={{ color: "var(--accent)" }}>الدفعات لا تُنقَل أبداً</b>
                </span>
              </div>
            </div>

            {/* The non-obvious part, said out loud. */}
            <div className="callout callout--note" style={{ marginTop: 12 }}>
              الدفعات تبقى على الحساب القديم لأن <b>الوصولات المطبوعة تشير إليها</b>. الذي ينتقل
              هو الرصيد، بقيدين ظاهرين — قيد سالب على القديم وقيد موجب على الجديد — لا الدفعة
              نفسها.
            </div>

            <div className="cluster" style={{ marginTop: 12 }}>
              <Button variant="primary" onClick={() => setStep(3)}>
                فهمت — التالي
              </Button>
              <Button variant="ghost" onClick={() => setStep(1)}>
                رجوع
              </Button>
            </div>
          </>
        )}

        {step === 3 && (
          <>
            <label className="field">
              <span className="field__label">سبب الاستبدال (إلزامي)</span>
              <input
                className="input"
                autoFocus
                value={reason}
                onChange={(e) => setReason(e.target.value)}
                placeholder="مثلاً: نُقل من المسائي إلى الصباحي بقرار العمادة"
              />
            </label>

            {refusal && (
              <div style={{ marginBottom: 10 }}>
                <RefusalPanel refusal={refusal} />
              </div>
            )}

            <div className="cluster">
              <Button
                variant="danger"
                disabled={reason.trim().length < 3}
                disabledReason={reason.trim().length < 3 ? "اكتب سبباً" : undefined}
                busy={run.isPending}
                onClick={() => run.mutate()}
              >
                تنفيذ الاستبدال
              </Button>
              <Button variant="ghost" onClick={() => setStep(2)}>
                رجوع
              </Button>
            </div>
          </>
        )}
      </Panel>
    </div>
  );
}

function ResultSheet({
  enrollment,
  onDone,
}: {
  enrollment: EnrollmentView;
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [result, setResult] = useState<"passed_r1" | "passed_r2" | "failed">("passed_r1");
  const [byDecision, setByDecision] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const record = useMutation({
    mutationFn: () =>
      api.post(`/enrollments/${enrollment.id}/result`, {
        result,
        by_decision: byDecision,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["enrollment"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <div style={{ marginTop: 12 }}>
      <Panel title="تسجيل نتيجة">
        <label className="field">
          <span className="field__label">النتيجة</span>
          <div className="cluster">
            {(
              [
                ["passed_r1", "ناجح — دور أول"],
                ["passed_r2", "ناجح — دور ثانٍ"],
                ["failed", "راسب"],
              ] as const
            ).map(([value, label]) => (
              <button
                key={value}
                type="button"
                className={`btn${result === value ? " btn--primary" : ""}`}
                onClick={() => setResult(value)}
              >
                {label}
              </button>
            ))}
          </div>
          <span className="field__hint">
            الدور الأول والثاني مفصولان: نتائج الدور الثاني تصل بعد إغلاق الخزينة بأسابيع.
          </span>
        </label>

        <label className="field" style={{ display: "flex", gap: 8, alignItems: "center" }}>
          <input
            type="checkbox"
            checked={byDecision}
            onChange={(e) => setByDecision(e.target.checked)}
          />
          <span className="field__label" style={{ margin: 0 }}>
            بقرار (لا بامتحان)
          </span>
        </label>

        {refusal && (
          <div style={{ marginBottom: 10 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}

        <div className="cluster">
          <Button variant="primary" busy={record.isPending} onClick={() => record.mutate()}>
            حفظ النتيجة
          </Button>
          <Button variant="ghost" onClick={onDone}>
            صرف النظر
          </Button>
        </div>
      </Panel>
    </div>
  );
}

/**
 * A lifecycle change, and the financial treatment it forces a decision on.
 *
 * There is no default treatment, and the screen does not invent one: charging
 * a student who withdrew and waiving what they owe are both defensible, and
 * only the university can choose. Omitting it is refused with the options
 * named, so they are named here instead.
 */
function StatusSheet({
  enrollment,
  onDone,
}: {
  enrollment: EnrollmentView;
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [status, setStatus] = useState("deferred");
  const [treatment, setTreatment] = useState("");
  const [reason, setReason] = useState("");
  const [result, setResult] = useState<ChangeStatusResponse | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const needsTreatment = ["deferred", "withdrawn", "dropped", "transferred"].includes(status);

  const change = useMutation({
    mutationFn: () =>
      api.post<ChangeStatusResponse>(`/enrollments/${enrollment.id}/status`, {
        status,
        ...(reason.trim() ? { reason: reason.trim() } : {}),
        ...(needsTreatment && treatment ? { financial_treatment: treatment } : {}),
      }),
    onSuccess: (data) => {
      setResult(data);
      void queryClient.invalidateQueries({ queryKey: ["enrollment"] });
      void queryClient.invalidateQueries({ queryKey: ["student-accounts"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (result) {
    return (
      <div style={{ marginTop: 12 }}>
        <Panel title="نُفِّذ التغيير">
          <Row label="الحالة الجديدة">
            <StateChip entity="enrollment" status={result.enrollment.status} />
          </Row>
          {result.financial_treatment && (
            <>
              <Row label="المعالجة المالية">{result.financial_treatment.treatment}</Row>
              <Row label="المُعفى">
                <Money value={result.financial_treatment.waived} tone="plain" />
              </Row>
              <Row label="رصيد دائن نشأ">
                <Money value={result.financial_treatment.credit_raised} tone="plain" />
              </Row>
              <Row label="الالتزام المتبقي">
                <Money value={result.financial_treatment.remaining_obligation} />
              </Row>
            </>
          )}
          {result.graduation_clearance && (
            <Row label="براءة الذمة">
              {result.graduation_clearance.cleared ? (
                <Chip tone="live">صادرة</Chip>
              ) : (
                <>
                  <Chip tone="void">محجوبة</Chip>
                  <Money value={result.graduation_clearance.outstanding} />
                </>
              )}
            </Row>
          )}
          <Button variant="ghost" onClick={onDone}>
            إغلاق
          </Button>
        </Panel>
      </div>
    );
  }

  return (
    <div style={{ marginTop: 12 }}>
      <Panel title="تغيير الحالة">
        <label className="field">
          <span className="field__label">الحالة الجديدة</span>
          <select className="input" value={status} onChange={(e) => setStatus(e.target.value)}>
            <option value="deferred">مؤجَّل</option>
            <option value="withdrawn">منسحب</option>
            <option value="dropped">منقطع</option>
            <option value="transferred">منقول</option>
            <option value="completed">مكتمل</option>
          </select>
        </label>

        {needsTreatment && (
          <label className="field">
            <span className="field__label">المعالجة المالية (إلزامية)</span>
            <select
              className="input"
              value={treatment}
              onChange={(e) => setTreatment(e.target.value)}
            >
              <option value="">— اختر —</option>
              <option value="keep">إبقاء الالتزام كاملاً</option>
              <option value="waive_unpaid">إعفاء غير المدفوع</option>
              <option value="waive_all">إعفاء الكل</option>
              <option value="partial">جزئي</option>
            </select>
            {/* No default, on purpose. */}
            <span className="field__hint">
              لا يوجد افتراضي: تحميل المنسحب رسومه وإعفاؤه منها كلاهما موقف يُدافَع عنه، والجامعة
              وحدها تختار.
            </span>
          </label>
        )}

        <label className="field">
          <span className="field__label">السبب</span>
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
            disabled={needsTreatment && !treatment}
            disabledReason={needsTreatment && !treatment ? "اختر المعالجة المالية" : undefined}
            busy={change.isPending}
            onClick={() => change.mutate()}
          >
            تنفيذ
          </Button>
          <Button variant="ghost" onClick={onDone}>
            صرف النظر
          </Button>
        </div>
      </Panel>
    </div>
  );
}
