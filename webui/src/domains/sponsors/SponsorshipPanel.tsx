import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { SponsorView, SponsorshipView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

/**
 * A student's sponsorships — on the student file because the agreement is
 * about this person, while the sponsors screen is about the bodies.
 *
 * The settlement mode is the field that needs a sentence, not a dropdown
 * label: it decides who receives the debt letter if the money never comes.
 */
export function SponsorshipPanel({ studentId }: { studentId: string }) {
  const { can, reason } = useSession();
  const [granting, setGranting] = useState(false);

  const sponsorships = useQuery({
    queryKey: ["student-sponsorships", studentId],
    queryFn: () => api.get<SponsorshipView[]>(`/students/${studentId}/sponsorships`),
  });

  const sponsors = useQuery({
    queryKey: ["sponsors"],
    queryFn: () => api.get<SponsorView[]>("/sponsors"),
  });

  const nameOf = (id: string) => (sponsors.data ?? []).find((s) => s.id === id)?.name_ar ?? id;

  const rows = sponsorships.data ?? [];

  return (
    <Panel
      title="الكفالات"
      aside={
        <Button
          size="sm"
          disabled={!can("sponsorship.create")}
          disabledReason={reason("sponsorship.create")}
          onClick={() => setGranting((g) => !g)}
        >
          {granting ? "إغلاق" : "كفالة جديدة"}
        </Button>
      }
    >
      {granting && (
        <SponsorshipForm
          studentId={studentId}
          sponsors={sponsors.data ?? []}
          onDone={() => setGranting(false)}
        />
      )}

      {rows.length === 0 && !granting ? (
        <EmptyState kind="not-yet" title="لا كفالات على هذا الطالب" />
      ) : (
        rows.map((agreement) => (
          <SponsorshipRow
            key={agreement.id}
            agreement={agreement}
            sponsorName={nameOf(agreement.sponsor_id)}
            studentId={studentId}
          />
        ))
      )}
    </Panel>
  );
}

function SponsorshipRow({
  agreement,
  sponsorName,
  studentId,
}: {
  agreement: SponsorshipView;
  sponsorName: string;
  studentId: string;
}) {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const invalidate = () =>
    void queryClient.invalidateQueries({ queryKey: ["student-sponsorships", studentId] });

  const approve = useMutation({
    mutationFn: () => api.post(`/sponsorships/${agreement.id}/approve`, {}),
    onSuccess: invalidate,
    onError: (e) => isRefusal(e) && setRefusal(e),
  });
  const revoke = useMutation({
    mutationFn: () => api.post(`/sponsorships/${agreement.id}/revoke`, { reason: "أُلغيت الاتفاقية" }),
    onSuccess: invalidate,
    onError: (e) => isRefusal(e) && setRefusal(e),
  });

  const coverage =
    agreement.coverage_type === "full" ? (
      "تغطية كاملة"
    ) : agreement.coverage_type === "percentage" ? (
      `${((agreement.coverage_bp ?? 0) / 100).toFixed(0)}٪ من الأساس القابل للخصم`
    ) : (
      <Money value={agreement.coverage_amount ?? 0} tone="plain" />
    );

  return (
    <div className="row" style={{ alignItems: "flex-start" }}>
      <div className="grow">
        <b>{sponsorName}</b>{" "}
        <span className="label">
          {coverage} · من {agreement.from_year_code}
          {agreement.to_year_code ? ` إلى ${agreement.to_year_code}` : " — مفتوحة"}
        </span>
        <div className="label">
          {agreement.settlement_mode === "receivable"
            ? "ذمة على الكفيل — الطالب يبقى مديناً حتى يدفع الكفيل"
            : "يغطي الدين — دين الطالب ينخفض والجامعة تحمل تخلّف الكفيل"}
        </div>
        {refusal && (
          <div style={{ marginTop: 6 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}
      </div>
      <SponsorshipStatus status={agreement.status} />
      {agreement.status === "draft" && (
        <Button
          size="sm"
          variant="primary"
          disabled={!can("sponsor.manage")}
          disabledReason={reason("sponsor.manage")}
          busy={approve.isPending}
          onClick={() => approve.mutate()}
        >
          اعتماد
        </Button>
      )}
      {(agreement.status === "active" || agreement.status === "approved") && (
        <Button
          size="sm"
          variant="danger"
          disabled={!can("sponsor.manage")}
          disabledReason={reason("sponsor.manage")}
          busy={revoke.isPending}
          onClick={() => revoke.mutate()}
        >
          سحب
        </Button>
      )}
    </div>
  );
}

function SponsorshipStatus({ status }: { status: string }) {
  switch (status) {
    case "active":
    case "approved":
      return <Chip tone="live">فعّالة</Chip>;
    case "draft":
      return <Chip tone="pending">مسودة — تنتظر اعتماد المدير المالي</Chip>;
    case "revoked":
      return <Chip tone="void">مسحوبة</Chip>;
    case "expired":
      return <Chip tone="muted">منتهية</Chip>;
    default:
      return <Chip tone="muted">{status}</Chip>;
  }
}

function SponsorshipForm({
  studentId,
  sponsors,
  onDone,
}: {
  studentId: string;
  sponsors: SponsorView[];
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const { years, activeYear } = useWorkingContext();
  const [sponsorId, setSponsorId] = useState("");
  const [coverageType, setCoverageType] = useState<"percentage" | "fixed_per_year" | "full">(
    "percentage",
  );
  const [percent, setPercent] = useState("100");
  const [fixed, setFixed] = useState("");
  const [mode, setMode] = useState<"" | "receivable" | "covers_debt">("");
  const [fromYear, setFromYear] = useState(activeYear?.code ?? "");
  const [agreementRef, setAgreementRef] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<SponsorshipView>("/sponsorships", {
        sponsor_id: sponsorId,
        student_id: studentId,
        coverage_type: coverageType,
        ...(coverageType === "percentage"
          ? { coverage_bp: Math.round(Number(percent) * 100) }
          : {}),
        ...(coverageType === "fixed_per_year"
          ? { coverage_amount: Number(fixed.replace(/,/g, "")) }
          : {}),
        settlement_mode: mode,
        from_year_code: fromYear,
        ...(agreementRef.trim() ? { agreement_ref: agreementRef.trim() } : {}),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["student-sponsorships", studentId] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready = Boolean(sponsorId && mode && fromYear);

  return (
    <div style={{ borderBottom: "1px solid var(--rule)", paddingBottom: 12, marginBottom: 10 }}>
      <label className="field">
        <span className="field__label">الكفيل</span>
        <select className="input" value={sponsorId} onChange={(e) => setSponsorId(e.target.value)}>
          <option value="">— اختر —</option>
          {sponsors
            .filter((s) => s.is_active)
            .map((s) => (
              <option key={s.id} value={s.id}>
                {s.name_ar}
              </option>
            ))}
        </select>
      </label>

      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">نوع التغطية</span>
          <select
            className="input"
            value={coverageType}
            onChange={(e) => setCoverageType(e.target.value as typeof coverageType)}
          >
            <option value="percentage">نسبة من الأساس القابل للخصم</option>
            <option value="fixed_per_year">مبلغ ثابت لكل سنة</option>
            <option value="full">تغطية كاملة</option>
          </select>
        </label>
        {coverageType === "percentage" && (
          <label className="field">
            <span className="field__label">النسبة ٪</span>
            <input
              className="input ltr"
              inputMode="decimal"
              value={percent}
              onChange={(e) => setPercent(e.target.value)}
            />
            <span className="field__hint">
              من الأساس القابل للخصم — «ثمانون بالمئة من الرسوم» لا تشمل الهوية.
            </span>
          </label>
        )}
        {coverageType === "fixed_per_year" && (
          <label className="field">
            <span className="field__label">المبلغ (دينار)</span>
            <input
              className="input ltr"
              inputMode="numeric"
              value={fixed}
              onChange={(e) => setFixed(e.target.value)}
            />
          </label>
        )}
      </div>

      <label className="field">
        <span className="field__label">نمط التسوية — لا افتراضي، لأنه يقرر من يُطالَب</span>
        <div className="cluster">
          <button
            type="button"
            className={`btn${mode === "receivable" ? " btn--primary" : ""}`}
            onClick={() => setMode("receivable")}
          >
            ذمة على الكفيل
          </button>
          <button
            type="button"
            className={`btn${mode === "covers_debt" ? " btn--primary" : ""}`}
            onClick={() => setMode("covers_debt")}
          >
            يغطي الدين
          </button>
        </div>
        <span className="field__hint">
          {mode === "receivable"
            ? "الطالب يبقى مديناً حتى يدفع الكفيل، وحصة الكفيل وارد متوقع يظهر في مستحقات الكفلاء."
            : mode === "covers_debt"
              ? "دين الطالب ينخفض فوراً، وإن تخلّف الكفيل حملت الجامعة الفرق."
              : "اختر أحد النمطين — الفرق هو من يستلم كتاب المطالبة."}
        </span>
      </label>

      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">من سنة</span>
          <select className="input" value={fromYear} onChange={(e) => setFromYear(e.target.value)}>
            {years.map((y) => (
              <option key={y.id} value={y.code}>
                {y.code}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field__label">مرجع الاتفاقية</span>
          <input
            className="input"
            value={agreementRef}
            onChange={(e) => setAgreementRef(e.target.value)}
            placeholder="كتاب الوزارة 1234 في …"
          />
        </label>
      </div>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!ready}
          disabledReason={!mode ? "اختر نمط التسوية" : !sponsorId ? "اختر الكفيل" : undefined}
          busy={create.isPending}
          onClick={() => create.mutate()}
        >
          تسجيل الكفالة — مسودة
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
        <span className="grow" />
        <span className="note">تبدأ مسودةً وتنتظر اعتماد المدير المالي.</span>
      </div>
    </div>
  );
}
