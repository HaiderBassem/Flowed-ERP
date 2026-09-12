import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { SponsorReceivableView, SponsorView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { amount, sum } from "@/lib/money";

const SPONSOR_TYPES = [
  { code: "ministry", label: "وزارة" },
  { code: "government", label: "جهة حكومية" },
  { code: "company", label: "شركة" },
  { code: "charity", label: "جهة خيرية" },
  { code: "individual", label: "فرد" },
  { code: "other", label: "أخرى" },
];

/**
 * Sponsors — the bodies that pay students' fees — and what each one owes.
 *
 * The receivables table is the reason this screen exists: a ministry that
 * committed to eighty students' fees is a debtor like any other, and a debt
 * nobody can see is a debt nobody collects. Committed, paid and outstanding
 * sit side by side per sponsor per year, exportable for the letter that goes
 * to the ministry.
 *
 * Granting a sponsorship to a student happens on the student's own file —
 * the agreement is about a person, and this screen is about the bodies.
 */
export function SponsorsScreen() {
  const { can, reason } = useSession();
  const { activeYear } = useWorkingContext();
  const [creating, setCreating] = useState(false);

  const sponsors = useQuery({
    queryKey: ["sponsors"],
    queryFn: () => api.get<SponsorView[]>("/sponsors"),
  });

  const receivables = useQuery({
    queryKey: ["sponsors", "receivables", activeYear?.id],
    queryFn: () =>
      api.get<SponsorReceivableView[]>("/sponsors/receivables", {
        query: { academic_year_id: activeYear?.id },
      }),
    enabled: Boolean(activeYear),
  });

  const rows = receivables.data ?? [];
  const outstanding = sum(rows.map((r) => amount(r.outstanding)));

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">الكفلاء</h1>
        {activeYear && <span className="num">{activeYear.code}</span>}
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("sponsor.manage")}
          disabledReason={reason("sponsor.manage")}
          onClick={() => setCreating((c) => !c)}
        >
          {creating ? "إغلاق" : "كفيل جديد"}
        </Button>
      </div>

      {creating && <SponsorForm onDone={() => setCreating(false)} />}

      <div className="deck" style={{ marginTop: 12 }}>
        <Stat value={(sponsors.data ?? []).length} label="كفيلاً مسجّلاً" />
        <Stat value={rows.length} label="كفيلاً عليه التزامات هذه السنة" />
        <Stat
          value={<Money value={outstanding} tone="plain" />}
          label="مستحق على الكفلاء — دين كأي دين"
          tone={outstanding > 0n ? "pending" : "live"}
        />
      </div>

      <div style={{ marginTop: 12 }}>
        <Panel
          title="مستحقات الكفلاء"
          aside={<span className="label">الملتزَم به مقابل ما وصل فعلاً</span>}
          flush
        >
          {receivables.isLoading && <Skeleton height={140} />}
          {receivables.isError &&
            (isRefusal(receivables.error) ? (
              <RefusalPanel
                refusal={receivables.error}
                onRetry={() => void receivables.refetch()}
              />
            ) : (
              <EmptyState kind="no-results" title="تعذّر جلب المستحقات" />
            ))}
          {receivables.isSuccess && rows.length === 0 && (
            <EmptyState
              kind="not-yet"
              title="لا التزامات كفلاء في هذه السنة"
              detail="الكفالة تُمنح من ملف الطالب، وتظهر التزاماتها هنا مجمّعة بالكفيل."
            />
          )}
          {rows.length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>الكفيل</th>
                  <th className="n">طلبة</th>
                  <th className="n">التزامات</th>
                  <th className="n col-group-money">الملتزَم به</th>
                  <th className="n">المدفوع</th>
                  <th className="n">المستحق</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((row) => (
                  <tr key={`${row.sponsor_id}-${row.academic_year_id}`}>
                    <td>
                      <b>{row.sponsor_name}</b>{" "}
                      <span className="k ltr">{row.sponsor_code}</span>
                    </td>
                    <td className="n">{row.student_count}</td>
                    <td className="n">{row.commitment_count}</td>
                    <td className="n col-group-money">
                      <Money value={row.committed} tone="plain" />
                    </td>
                    <td className="n">
                      <Money value={row.paid} tone="positive" />
                    </td>
                    <td className="n">
                      <Money value={row.outstanding} />
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>

      <div style={{ marginTop: 12 }}>
        <Panel title="الكفلاء المسجّلون" flush>
          {sponsors.isLoading && <Skeleton height={120} />}
          {sponsors.isSuccess && (sponsors.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا كفلاء مسجّلين" />
          )}
          {(sponsors.data ?? []).length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>الرمز</th>
                  <th>الاسم</th>
                  <th>النوع</th>
                  <th>جهة الاتصال</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {(sponsors.data ?? []).map((sponsor) => (
                  <tr key={sponsor.id}>
                    <td className="k ltr">{sponsor.code}</td>
                    <td>{sponsor.name_ar}</td>
                    <td>
                      {SPONSOR_TYPES.find((t) => t.code === sponsor.sponsor_type)?.label ??
                        sponsor.sponsor_type}
                    </td>
                    <td>
                      {sponsor.contact_name ?? "—"}
                      {sponsor.contact_phone && (
                        <span className="label num"> · {sponsor.contact_phone}</span>
                      )}
                    </td>
                    <td>
                      {sponsor.is_active ? (
                        <Chip tone="live">فعّال</Chip>
                      ) : (
                        <Chip tone="muted">معطّل</Chip>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>

      <p className="note" style={{ marginTop: 12, maxWidth: "72ch" }}>
        نمط التسوية في كل اتفاقية هو ما يقرر من يستلم كتاب المطالبة: «ذمة على الكفيل» تُبقي
        الطالب مديناً وتحتسب حصة الكفيل وارداً متوقعاً؛ «يغطي الدين» يخفض دين الطالب وتحمل
        الجامعة تخلّف الكفيل.
      </p>
    </main>
  );
}

function SponsorForm({ onDone }: { onDone: () => void }) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [type, setType] = useState("ministry");
  const [contactName, setContactName] = useState("");
  const [contactPhone, setContactPhone] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<SponsorView>("/sponsors", {
        code: code.trim(),
        name_ar: name.trim(),
        sponsor_type: type,
        ...(contactName.trim() ? { contact_name: contactName.trim() } : {}),
        ...(contactPhone.trim() ? { contact_phone: contactPhone.trim() } : {}),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["sponsors"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="كفيل جديد">
      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">الرمز</span>
          <input className="input ltr" value={code} onChange={(e) => setCode(e.target.value)} />
        </label>
        <label className="field">
          <span className="field__label">الاسم</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
      </div>
      <div className="cols cols--thirds">
        <label className="field">
          <span className="field__label">النوع</span>
          <select className="input" value={type} onChange={(e) => setType(e.target.value)}>
            {SPONSOR_TYPES.map((t) => (
              <option key={t.code} value={t.code}>
                {t.label}
              </option>
            ))}
          </select>
        </label>
        <label className="field">
          <span className="field__label">جهة الاتصال</span>
          <input
            className="input"
            value={contactName}
            onChange={(e) => setContactName(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field__label">هاتف الاتصال</span>
          <input
            className="input ltr"
            value={contactPhone}
            onChange={(e) => setContactPhone(e.target.value)}
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
          disabled={!code.trim() || !name.trim()}
          busy={create.isPending}
          onClick={() => create.mutate()}
        >
          تسجيل الكفيل
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
