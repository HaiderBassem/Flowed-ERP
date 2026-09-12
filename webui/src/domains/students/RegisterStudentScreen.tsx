import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { StudentView } from "@/api/types";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Crumbs } from "@/components/Crumbs";
import { Button, Panel } from "@/components/primitives";
import { useSession } from "@/app/session";
import { fold, normalisePhone } from "@/lib/text";

/**
 * Registering a student identity.
 *
 * The student row holds identity only — no money, no year, nothing that could
 * differ between two academic years. That is the point of the split, and the
 * screen says so at the end so nobody goes looking for a fees section.
 *
 * The mother's name is required, not optional: it is what distinguishes two
 * students whose names fold to the same string, and §06 shows exactly that
 * case. The duplicate acknowledgement is a recorded override — a wrongly
 * created second record for one person can be traced to the decision that made
 * it.
 */
export function RegisterStudentScreen() {
  const navigate = useNavigate();
  const { can, reason } = useSession();

  const [studentNo, setStudentNo] = useState("");
  const [fullName, setFullName] = useState("");
  const [motherName, setMotherName] = useState("");
  const [nationalId, setNationalId] = useState("");
  const [phone, setPhone] = useState("");
  const [birthDate, setBirthDate] = useState("");
  const [gender, setGender] = useState("");
  const [acknowledge, setAcknowledge] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const register = useMutation({
    mutationFn: () =>
      api.post<StudentView>("/students", {
        student_no: studentNo.trim(),
        full_name: fullName.trim(),
        mother_name: motherName.trim(),
        ...(nationalId.trim() ? { national_id: nationalId.trim() } : {}),
        ...(phone.trim() ? { phone: normalisePhone(phone) } : {}),
        ...(birthDate ? { birth_date: birthDate } : {}),
        ...(gender ? { gender } : {}),
        ...(acknowledge ? { acknowledge_duplicates: true } : {}),
      }),
    onSuccess: (student) => navigate(`/students/${student.id}`),
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const duplicateRefusal =
    refusal !== null && /duplicate|probable_match/i.test(refusal.code);

  const ready = studentNo.trim() && fullName.trim() && motherName.trim();

  return (
    <main className="screen">
      <Crumbs items={[{ label: "الطلبة", to: "/students" }, { label: "تسجيل طالب جديد" }]} />
      <div className="screen__head">
        <h1 className="screen__title">تسجيل طالب جديد</h1>
      </div>

      <div style={{ maxWidth: 620, marginTop: 12 }}>
        <Panel title="الهوية">
          <div className="cols cols--half">
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
              <span className="field__label">الرقم الوطني</span>
              <input
                className="input ltr"
                value={nationalId}
                onChange={(e) => setNationalId(e.target.value)}
              />
            </label>
          </div>

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
            <label className="field">
              <span className="field__label">الهاتف</span>
              <input
                className="input ltr"
                value={phone}
                onChange={(e) => setPhone(e.target.value)}
                placeholder="07XXXXXXXXX"
              />
              {phone.trim() && (
                <span className="field__hint ltr">{normalisePhone(phone)}</span>
              )}
            </label>
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

          <p className="note" style={{ marginTop: 10 }}>
            هذه هوية فقط — لا سنة ولا رسوم ولا حساب. الوحدة المالية هي التسجيل في سنة، وهو أمر
            تالٍ منفصل.
          </p>
        </Panel>
      </div>
    </main>
  );
}
