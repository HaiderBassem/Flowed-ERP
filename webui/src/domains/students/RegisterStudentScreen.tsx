import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { RegisterStudentWithPlacementView } from "@/api/types";
import { departmentsOf, useColleges, useDepartments, useStudyTypes } from "@/api/reference";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { Button, Panel, Row } from "@/components/primitives";
import { IraqiPhoneInput, isCompleteIraqiMobile, toApiPhone } from "@/components/IraqiPhoneInput";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { amount } from "@/lib/money";
import { fold } from "@/lib/text";

/**
 * Registering a student identity and placing it into a year — one screen,
 * one submission.
 *
 * The student row itself still holds identity only — no money, nothing that
 * could differ between two academic years lives there. Study type, academic
 * year, department and stage all live on the enrollment this screen creates
 * alongside the identity, through POST /students/intake. Nothing about that
 * split changes: what changes is that the operator no longer has to know it
 * exists to register one student in one sitting.
 *
 * Pricing follows the enrollment automatically **only when the signed-in
 * operator also holds finance authority** — generating an account has always
 * been a separate, finance-manager-only action (see the enrollment screen),
 * and this form does not quietly cross that line. A registrar without that
 * authority still gets the identity and the enrollment; the result panel
 * says a finance manager finishes the pricing.
 *
 * The mother's name is required, not optional: it is what distinguishes two
 * students whose names fold to the same string, and §06 shows exactly that
 * case. The duplicate acknowledgement is a recorded override — a wrongly
 * created second record for one person can be traced to the decision that
 * made it.
 */
export function RegisterStudentScreen() {
  const navigate = useNavigate();
  const { can, reason } = useSession();
  const { activeYear, years } = useWorkingContext();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const [studentNo, setStudentNo] = useState("");
  const [fullName, setFullName] = useState("");
  const [motherName, setMotherName] = useState("");
  // Subscriber digits only (no leading 0, no +964) — @/components/IraqiPhoneInput.
  const [phone, setPhone] = useState("");
  const [birthDate, setBirthDate] = useState("");
  const [gender, setGender] = useState("");
  const [acknowledge, setAcknowledge] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const [yearId, setYearId] = useState(activeYear?.id ?? "");
  const [college, setCollege] = useState("");
  const [department, setDepartment] = useState("");
  const [studyType, setStudyType] = useState("");
  const [stage, setStage] = useState("1");

  const [result, setResult] = useState<RegisterStudentWithPlacementView | null>(null);

  const register = useMutation({
    mutationFn: () =>
      api.post<RegisterStudentWithPlacementView>("/students/intake", {
        student_no: studentNo.trim(),
        full_name: fullName.trim(),
        mother_name: motherName.trim(),
        ...(phone ? { phone: toApiPhone(phone) } : {}),
        ...(birthDate ? { birth_date: birthDate } : {}),
        ...(gender ? { gender } : {}),
        ...(acknowledge ? { acknowledge_duplicates: true } : {}),
        academic_year_id: yearId,
        department_id: department,
        study_type_id: studyType,
        stage: Number(stage),
      }),
    onSuccess: (data) => {
      setResult(data);
      setRefusal(null);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const duplicateRefusal =
    refusal !== null && /duplicate|probable_match/i.test(refusal.code);

  const ready =
    Boolean(studentNo.trim() && fullName.trim() && motherName.trim()) &&
    (phone.length === 0 || isCompleteIraqiMobile(phone)) &&
    Boolean(yearId && department && studyType && stage);

  const studyTypeName = (id: string) => studyTypes.data?.find((s) => s.id === id)?.name_ar ?? "—";
  const yearCode = (id: string) => years.find((y) => y.id === id)?.code ?? "—";

  if (result) {
    return (
      <main className="screen">
        <Crumbs items={[{ label: "الطلبة", to: "/students" }, { label: "تسجيل طالب جديد" }]} />
        <div className="screen__head">
          <h1 className="screen__title">تم التسجيل</h1>
        </div>
        <div style={{ maxWidth: 560, marginTop: 12 }}>
          <Panel title={`${result.student.full_name} — ${result.student.student_no}`}>
            <Row label="السنة الدراسية">
              <span className="num">{yearCode(result.enrollment.academic_year_id)}</span>
            </Row>
            <Row label="نوع الدراسة">{studyTypeName(result.enrollment.study_type_id)}</Row>
            <Row label="المرحلة">
              <span className="num">{result.enrollment.stage}</span>
            </Row>

            {result.account ? (
              <Row label="الدين الابتدائي">
                <Money value={amount(result.account.remaining)} size="huge" />
              </Row>
            ) : (
              <div className="refusal refusal--pending" style={{ marginTop: 10 }}>
                <p className="refusal__title">التسعير لم يتم بعد</p>
                <p className="refusal__body" style={{ margin: 0 }}>
                  {result.pricing_note ??
                    "التسجيل تم بلا تسعير — التسعير أمر منفصل يقوم به المدير المالي."}
                </p>
              </div>
            )}

            <div className="cluster" style={{ marginTop: 14 }}>
              <Button variant="primary" onClick={() => navigate(`/students/${result.student.id}`)}>
                فتح ملف الطالب
              </Button>
              <Button
                variant="ghost"
                onClick={() => {
                  setResult(null);
                  setStudentNo("");
                  setFullName("");
                  setMotherName("");
                  setPhone("");
                  setBirthDate("");
                  setGender("");
                  setAcknowledge(false);
                }}
              >
                تسجيل طالب آخر
              </Button>
            </div>
          </Panel>
        </div>
      </main>
    );
  }

  return (
    <main className="screen">
      <Crumbs items={[{ label: "الطلبة", to: "/students" }, { label: "تسجيل طالب جديد" }]} />
      <div className="screen__head">
        <h1 className="screen__title">تسجيل طالب جديد</h1>
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <Panel title="الهوية">
          <label className="field">
            <span className="field__label">الرقم الجامعي</span>
            <input
              className="input ltr"
              autoFocus
              value={studentNo}
              onChange={(e) => setStudentNo(e.target.value)}
            />
          </label>

          <label className="field">
            <span className="field__label">الاسم الكامل</span>
            <input
              className="input"
              value={fullName}
              onChange={(e) => setFullName(e.target.value)}
            />
            {fullName.trim() && fold(fullName) !== fullName.trim() && (
              <span className="field__hint">
                سيُخزَّن مطبَّعاً للبحث كـ <span className="ltr">{fold(fullName)}</span> — الأصل
                يبقى كما كُتب.
              </span>
            )}
          </label>

          <label className="field">
            <span className="field__label">اسم الأم</span>
            <input
              className="input"
              value={motherName}
              onChange={(e) => setMotherName(e.target.value)}
            />
            {/* Not a secondary field. */}
            <span className="field__hint">
              إلزامي: اسمان متطابقان بعد التطبيع تفرّق بينهما الأم، ولذلك هو عمود دائم في كل
              قائمة.
            </span>
          </label>

          <div className="cols cols--thirds">
            <IraqiPhoneInput label="الهاتف" value={phone} onChange={setPhone} />
            <label className="field">
              <span className="field__label">تاريخ الميلاد</span>
              <input
                className="input num"
                type="date"
                value={birthDate}
                onChange={(e) => setBirthDate(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field__label">الجنس</span>
              <select className="input" value={gender} onChange={(e) => setGender(e.target.value)}>
                <option value="">—</option>
                <option value="male">ذكر</option>
                <option value="female">أنثى</option>
              </select>
            </label>
          </div>
        </Panel>

        <Panel title="السياق الأكاديمي">
          <label className="field">
            <span className="field__label">السنة الدراسية</span>
            <select className="input" value={yearId} onChange={(e) => setYearId(e.target.value)}>
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

          <p className="note" style={{ marginBottom: 10 }}>
            التسعير يتبع تلقائياً إن كانت صلاحيتك تشمل المدير المالي أو المدير العام؛ غير ذلك
            يبقى التسجيل بلا حساب حتى يسعّره المدير المالي — كما كان الحال دوماً بين التسجيل
            وتوليد الحساب.
          </p>
        </Panel>
      </div>

      <div style={{ maxWidth: 620, marginTop: 12 }}>
        {refusal && (
          <div style={{ marginBottom: 12 }}>
            <RefusalPanel
              refusal={refusal}
              {...(duplicateRefusal
                ? {
                    actions: [
                      {
                        label: "هذا شخص مختلف — سجّله",
                        primary: true,
                        onClick: () => {
                          setAcknowledge(true);
                          register.mutate();
                        },
                      },
                    ],
                  }
                : {})}
            />
            {duplicateRefusal && (
              <p className="note" style={{ marginTop: 6 }}>
                التجاوز يُسجَّل باسمك، فسجلٌّ ثانٍ أُنشئ لشخص واحد يمكن تتبّعه إلى القرار الذي
                أنشأه.
              </p>
            )}
          </div>
        )}

        <Button
          variant="primary"
          size="lg"
          disabled={!can("student.register") || !ready}
          disabledReason={
            !can("student.register") ? reason("student.register") : !ready ? "أكمل الحقول الإلزامية" : undefined
          }
          busy={register.isPending}
          onClick={() => register.mutate()}
        >
          تسجيل
        </Button>
      </div>
    </main>
  );
}
