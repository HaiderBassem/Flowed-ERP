import { useEffect, useMemo, useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { departmentsOf, useColleges, useDepartments, useStudyTypes } from "@/api/reference";
import type { AccountView, EnrollmentView, Page, StudentView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { amount, sum } from "@/lib/money";
import { fold } from "@/lib/text";

/**
 * Enrolling a student in a year — §09.
 *
 * The rule that shapes it: the debt-block check is shown **before** the
 * request is sent, under the year's own policy — ignore, warn or block — and
 * an override requires a written reason that is recorded. A clerk who
 * discovers the block only from a server refusal has already spent the time,
 * and has no way to see how much debt caused it.
 *
 * The debt figure here is the student's existing accounts, which the interface
 * can read. The decision itself remains the server's: this screen shows what
 * the policy will weigh, it does not pre-empt it.
 */
export function EnrollScreen() {
  const navigate = useNavigate();
  const [params] = useSearchParams();
  const { can, reason } = useSession();
  const { activeYear, years } = useWorkingContext();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const [term, setTerm] = useState("");
  const [studentId, setStudentId] = useState(params.get("student") ?? "");
  const [yearId, setYearId] = useState("");
  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [studyType, setStudyType] = useState("");
  const [stage, setStage] = useState("1");
  const [override, setOverride] = useState(false);
  const [overrideReason, setOverrideReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  useEffect(() => {
    if (!yearId && activeYear) setYearId(activeYear.id);
  }, [activeYear, yearId]);

  const debounced = useDebounced(term, 250);
  const results = useQuery({
    queryKey: ["students", "enroll", debounced],
    queryFn: () => api.get<Page<StudentView>>("/students", { query: { q: debounced, limit: 8 } }),
    enabled: debounced.trim().length >= 2 && !studentId,
  });

  const student = useQuery({
    queryKey: ["student", studentId],
    queryFn: () => api.get<StudentView>(`/students/${studentId}`),
    enabled: Boolean(studentId),
  });

  const accounts = useQuery({
    queryKey: ["student-accounts", studentId],
    queryFn: () => api.get<AccountView[]>(`/students/${studentId}/accounts`),
    enabled: Boolean(studentId),
  });

  const outstanding = useMemo(
    () =>
      sum(
        (accounts.data ?? [])
          .filter((a) => a.status !== "cancelled")
          .map((a) => amount(a.remaining)),
      ),
    [accounts.data],
  );

  const year = years.find((y) => y.id === yearId);
  const policy = year?.debt_block_policy ?? "warn";
  const hasDebt = outstanding > 0n;

  const enroll = useMutation({
    mutationFn: () =>
      api.post<EnrollmentView>("/enrollments", {
        student_id: studentId,
        academic_year_id: yearId,
        department_id: department,
        study_type_id: studyType,
        stage: Number(stage),
        ...(override
          ? { override_debt_block: true, override_reason: overrideReason.trim() }
          : {}),
      }),
    onSuccess: (created) => navigate(`/enrollments/${created.id}`),
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready =
    Boolean(studentId && yearId && department && studyType) &&
    (!override || overrideReason.trim().length >= 3);

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">تسجيل طالب في سنة</h1>
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <div className="stack">
          <Panel title="الطالب">
            {!studentId ? (
              <>
                <input
                  className="input"
                  autoFocus
                  placeholder="رقم جامعي أو اسم…"
                  value={term}
                  onChange={(e) => setTerm(e.target.value)}
                />
                {debounced.trim().length >= 2 && fold(debounced) !== debounced.trim() && (
                  <p className="note" style={{ marginTop: 6 }}>
                    طُبِّع البحث إلى <span className="ltr">{fold(debounced)}</span>
                  </p>
                )}
                <div style={{ marginTop: 8 }}>
                  {(results.data?.data ?? []).map((s) => (
                    <button
                      key={s.id}
                      type="button"
                      className="palette__row"
                      style={{ borderRadius: 6 }}
                      onClick={() => setStudentId(s.id)}
                    >
                      <span className="grow">
                        <b>{s.full_name}</b>{" "}
                        <span className="label">— الأم: {s.mother_name}</span>
                      </span>
                      <span className="num">{s.student_no}</span>
                    </button>
                  ))}
                  {results.isSuccess && (results.data?.data ?? []).length === 0 && (
                    <EmptyState kind="no-results" title="لا نتائج" />
                  )}
                </div>
              </>
            ) : (
              <>
                <Row label="الاسم">
                  <b>{student.data?.full_name ?? "…"}</b>
                </Row>
                <Row label="اسم الأم">{student.data?.mother_name ?? "…"}</Row>
                <Row label="الرقم الجامعي">
                  <span className="num">{student.data?.student_no ?? "…"}</span>
                </Row>
                <Button
                  size="sm"
                  variant="ghost"
                  onClick={() => {
                    setStudentId("");
                    setTerm("");
                  }}
                >
                  تغيير الطالب
                </Button>
              </>
            )}
          </Panel>

          {/* The debt check, before anything is sent. */}
          {studentId && (
            <Panel title="فحص حجب الدين">
              <Row label="سياسة السنة">
                <Chip
                  tone={policy === "block" ? "void" : policy === "warn" ? "pending" : "muted"}
                >
                  {policy === "block" ? "حجب" : policy === "warn" ? "تحذير" : "تجاهل"}
                </Chip>
              </Row>
              <Row label="الدين القائم">
                <Money value={outstanding} tone={hasDebt ? "auto" : "plain"} />
              </Row>

              {!hasDebt && (
                <p className="note">لا دين قائم — التسجيل يمر بلا اعتراض.</p>
              )}

              {hasDebt && policy === "ignore" && (
                <p className="note">
                  على الطالب دين، وسياسة هذه السنة تتجاهله. التسجيل يمر.
                </p>
              )}

              {hasDebt && policy !== "ignore" && (
                <>
                  <div
                    className={`refusal${policy === "warn" ? " refusal--pending" : ""}`}
                    style={{ marginTop: 8 }}
                  >
                    <p className="refusal__title">
                      {policy === "block" ? "التسجيل محجوب بالدين" : "تنبيه: على الطالب دين"}
                    </p>
                    <p className="refusal__body" style={{ margin: 0 }}>
                      {policy === "block"
                        ? "سياسة هذه السنة تحجب التسجيل حتى يُسدَّد الدين أو يُتجاوز الحجب بسبب مكتوب."
                        : "سياسة هذه السنة تحذّر ولا تحجب. التسجيل يمر، والدين يبقى على حسابه القديم."}
                    </p>
                  </div>

                  {policy === "block" && (
                    <label className="field" style={{ marginTop: 10 }}>
                      <span
                        className="field__label"
                        style={{ display: "flex", gap: 8, alignItems: "center" }}
                      >
                        <input
                          type="checkbox"
                          checked={override}
                          onChange={(e) => setOverride(e.target.checked)}
                        />
                        تجاوز الحجب
                      </span>
                      {override && (
                        <>
                          <input
                            className="input"
                            placeholder="سبب التجاوز (إلزامي، ويُسجَّل باسمك)"
                            value={overrideReason}
                            onChange={(e) => setOverrideReason(e.target.value)}
                          />
                          <span className="field__hint">
                            يُسجَّل في سجل التدقيق باسمك — التجاوز الصامت هو ما يجعل الحجب بلا
                            معنى.
                          </span>
                        </>
                      )}
                    </label>
                  )}
                </>
              )}
            </Panel>
          )}
        </div>

        <Panel title="السياق الأكاديمي">
          <label className="field">
            <span className="field__label">السنة</span>
            <select className="input" value={yearId} onChange={(e) => setYearId(e.target.value)}>
              {years.map((y) => (
                <option key={y.id} value={y.id}>
                  {y.code}
                </option>
              ))}
            </select>
            {year && !year.accepts_academic_recording && (
              <span className="field__error">
                هذه السنة لا تقبل التسجيل الأكاديمي — الطلب سيُرفض.
              </span>
            )}
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
              {departmentsOf(departments.data, college).map((d) => (
                <option key={d.id} value={d.id}>
                  {d.name_ar}
                </option>
              ))}
            </select>
          </label>

          <div className="cols cols--half">
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

          {/* Category is derived, not typed: a second attempt at a stage makes
              the student a repeat student, which usually prices differently. */}
          <p className="note" style={{ marginBottom: 10 }}>
            فئة الطالب تُشتق ولا تُكتب: محاولة ثانية على المرحلة نفسها تجعله «معيداً»، وهو ما
            تُسعِّر عليه سياسة المعيدين.
          </p>

          {refusal && (
            <div style={{ marginBottom: 10 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <Button
            variant="primary"
            size="lg"
            disabled={!can("enrollment.write") || !ready}
            disabledReason={
              !can("enrollment.write")
                ? reason("enrollment.write")
                : !studentId
                  ? "اختر طالباً"
                  : !ready
                    ? "أكمل الحقول"
                    : undefined
            }
            busy={enroll.isPending}
            onClick={() => enroll.mutate()}
          >
            تسجيل
          </Button>

          <p className="note" style={{ marginTop: 8 }}>
            التسجيل <b>لا يولّد حساباً مالياً</b>. التسعير أمر منفصل يقوم به المدير المالي.
          </p>
        </Panel>
      </div>
    </main>
  );
}

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return debounced;
}
