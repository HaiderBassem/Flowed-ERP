import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { StudentView } from "@/api/types";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Crumbs } from "@/components/Crumbs";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { IraqiPhoneInput, isCompleteIraqiMobile, subscriberDigitsOf, toApiPhone } from "@/components/IraqiPhoneInput";
import { useSession } from "@/app/session";

/**
 * Editing a student's contact details — PATCH /students/:id/contact.
 *
 * Only contact fields are editable here, on purpose: identity (name, mother's
 * name, national details) changes through the documented legal-change flow at
 * §"الهوية القانونية", and study type / academic year live on the enrollment,
 * not on the student — §09, and changed there via supersede.
 */
export function EditStudentScreen() {
  const { id } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { can, reason } = useSession();

  const student = useQuery({
    queryKey: ["student", id],
    queryFn: () => api.get<StudentView>(`/students/${id}`),
    enabled: Boolean(id),
  });

  const [phone, setPhone] = useState("");
  const [phoneAlt, setPhoneAlt] = useState("");
  const [email, setEmail] = useState("");
  const [address, setAddress] = useState("");
  const [guardianName, setGuardianName] = useState("");
  const [guardianPhone, setGuardianPhone] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [loaded, setLoaded] = useState(false);

  useEffect(() => {
    if (student.data && !loaded) {
      setPhone(subscriberDigitsOf(student.data.phone));
      setPhoneAlt(student.data.phone_alt ?? "");
      setEmail(student.data.email ?? "");
      setAddress(student.data.address ?? "");
      setGuardianName(student.data.guardian_name ?? "");
      setGuardianPhone(student.data.guardian_phone ?? "");
      setLoaded(true);
    }
  }, [student.data, loaded]);

  const save = useMutation({
    mutationFn: () =>
      api.patch<StudentView>(`/students/${id}/contact`, {
        phone: toApiPhone(phone) ?? "",
        phone_alt: phoneAlt.trim(),
        email: email.trim(),
        address: address.trim(),
        guardian_name: guardianName.trim(),
        guardian_phone: guardianPhone.trim(),
      }),
    onSuccess: (updated) => {
      queryClient.setQueryData(["student", id], updated);
      navigate(`/students/${id}`);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (student.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  if (student.isError || !student.data) {
    return (
      <main className="screen">
        {isRefusal(student.error) ? (
          <RefusalPanel refusal={student.error} onRetry={() => void student.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح ملف الطالب" />
        )}
      </main>
    );
  }

  const view = student.data;
  const ready = phone.length === 0 || isCompleteIraqiMobile(phone);

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: view.full_name, to: `/students/${view.id}` },
          { label: "تعديل بيانات التواصل" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">تعديل بيانات التواصل</h1>
      </div>

      <div style={{ maxWidth: 620, marginTop: 12 }}>
        <Panel title={`${view.full_name} — ${view.student_no}`}>
          <div className="cols cols--half">
            <IraqiPhoneInput label="الهاتف" value={phone} onChange={setPhone} />
            <label className="field">
              <span className="field__label">الهاتف البديل</span>
              <input
                className="input ltr"
                value={phoneAlt}
                onChange={(e) => setPhoneAlt(e.target.value)}
              />
            </label>
          </div>

          <label className="field">
            <span className="field__label">البريد الإلكتروني</span>
            <input
              className="input ltr"
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
            />
          </label>

          <label className="field">
            <span className="field__label">العنوان</span>
            <input className="input" value={address} onChange={(e) => setAddress(e.target.value)} />
          </label>

          <div className="cols cols--half">
            <label className="field">
              <span className="field__label">اسم ولي الأمر</span>
              <input
                className="input"
                value={guardianName}
                onChange={(e) => setGuardianName(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field__label">هاتف ولي الأمر</span>
              <input
                className="input ltr"
                value={guardianPhone}
                onChange={(e) => setGuardianPhone(e.target.value)}
              />
            </label>
          </div>

          {refusal && (
            <div style={{ marginBottom: 12 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <div className="cluster">
            <Button
              variant="primary"
              size="lg"
              disabled={!can("student.contact") || !ready}
              disabledReason={
                !can("student.contact")
                  ? reason("student.contact")
                  : !ready
                    ? "الهاتف المُدخل غير صالح"
                    : undefined
              }
              busy={save.isPending}
              onClick={() => save.mutate()}
            >
              حفظ
            </Button>
            <Link className="btn" to={`/students/${view.id}`}>
              إلغاء
            </Link>
          </div>

          <p className="note" style={{ marginTop: 10 }}>
            نوع الدراسة والسنة الدراسية يعودان للتسجيل لا للطالب — لتغييرهما افتح{" "}
            <Link to={`/students/${view.id}`}>السلسلة الأكاديمية</Link> واستخدم الاستبدال.
          </p>
        </Panel>
      </div>
    </main>
  );
}
