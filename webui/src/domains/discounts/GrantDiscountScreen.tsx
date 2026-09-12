import { useState } from "react";
import { useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { DiscountDefinitionView, StudentView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

type Scope = "single_year" | "year_range" | "all_years";

/**
 * Granting a discount to a student — §09.
 *
 * The rule that needs saying on screen: choosing "all years" does not discount
 * every future year outright. It creates an application against each new
 * account, and whether that application activates depends on the definition's
 * annual-reconfirmation flag. Until it is confirmed the fee stands at full.
 *
 * And the grantor does not approve their own grant. That is separation of
 * duties, not an oversight, so the screen states it rather than letting the
 * refusal explain it later.
 */
export function GrantDiscountScreen() {
  const { id: studentId } = useParams<{ id: string }>();
  const navigate = useNavigate();
  const { can, reason } = useSession();
  const { years, activeYear } = useWorkingContext();

  const [definitionId, setDefinitionId] = useState("");
  const [scope, setScope] = useState<Scope>("single_year");
  const [yearFrom, setYearFrom] = useState(activeYear?.id ?? "");
  const [yearTo, setYearTo] = useState("");
  const [justification, setJustification] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const student = useQuery({
    queryKey: ["student", studentId],
    queryFn: () => api.get<StudentView>(`/students/${studentId}`),
    enabled: Boolean(studentId),
  });

  const definitions = useQuery({
    queryKey: ["discount-definitions"],
    queryFn: () => api.get<DiscountDefinitionView[]>("/discounts/definitions"),
  });

  const definition = (definitions.data ?? []).find((d) => d.id === definitionId);

  const grant = useMutation({
    mutationFn: () =>
      api.post("/discounts/assignments", {
        student_id: studentId,
        definition_id: definitionId,
        scope,
        ...(scope !== "all_years" && yearFrom ? { year_from_id: yearFrom } : {}),
        ...(scope === "year_range" && yearTo ? { year_to_id: yearTo } : {}),
        ...(justification.trim() ? { justification: justification.trim() } : {}),
      }),
    onSuccess: () => navigate(`/students/${studentId}`),
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready = Boolean(definitionId && (scope === "all_years" || yearFrom));

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: student.data?.full_name ?? "ملف الطالب", to: `/students/${studentId}` },
          { label: "منح خصم" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">منح خصم</h1>
        {student.data && <span className="label">{student.data.full_name}</span>}
      </div>

      <div style={{ maxWidth: 640, marginTop: 12 }}>
        <Panel title="الخصم">
          <label className="field">
            <span className="field__label">التعريف</span>
            <select
              className="input"
              value={definitionId}
              onChange={(e) => setDefinitionId(e.target.value)}
            >
              <option value="">— اختر —</option>
              {(definitions.data ?? [])
                .filter((d) => d.is_active)
                .map((d) => (
                  <option key={d.id} value={d.id}>
                    {d.name_ar} ({d.code})
                  </option>
                ))}
            </select>
          </label>

          {definition && (
            <>
              <Row label="إعفاء كامل">{definition.is_full_exemption ? "نعم" : "لا"}</Row>
              <Row label="تأكيد سنوي">
                {definition.annual_reconfirmation ? (
                  <Chip tone="pending">مطلوب</Chip>
                ) : (
                  <span className="label">غير مطلوب</span>
                )}
              </Row>
            </>
          )}

          <label className="field" style={{ marginTop: 10 }}>
            <span className="field__label">النطاق الزمني</span>
            <div className="cluster">
              {(
                [
                  ["single_year", "سنة واحدة"],
                  ["year_range", "مدى سنوات"],
                  ["all_years", "كل السنوات"],
                ] as const
              ).map(([value, label]) => (
                <button
                  key={value}
                  type="button"
                  className={`btn${scope === value ? " btn--primary" : ""}`}
                  onClick={() => setScope(value)}
                >
                  {label}
                </button>
              ))}
            </div>
          </label>

          {scope !== "all_years" && (
            <div className="cols cols--half">
              <label className="field">
                <span className="field__label">من سنة</span>
                <select
                  className="input"
                  value={yearFrom}
                  onChange={(e) => setYearFrom(e.target.value)}
                >
                  <option value="">— اختر —</option>
                  {years.map((y) => (
                    <option key={y.id} value={y.id}>
                      {y.code}
                    </option>
                  ))}
                </select>
              </label>
              {scope === "year_range" && (
                <label className="field">
                  <span className="field__label">إلى سنة</span>
                  <select
                    className="input"
                    value={yearTo}
                    onChange={(e) => setYearTo(e.target.value)}
                  >
                    <option value="">— مفتوح —</option>
                    {years.map((y) => (
                      <option key={y.id} value={y.id}>
                        {y.code}
                      </option>
                    ))}
                  </select>
                </label>
              )}
            </div>
          )}

          {/* The consequence of "all years", stated explicitly. */}
          {scope === "all_years" && (
            <div className="callout callout--note" style={{ marginBottom: 12 }}>
              <b>ماذا يعني «كل السنوات» بالضبط:</b> يتولّد تطبيق خصم مع <b>كل حساب جديد</b>.
              {definition?.annual_reconfirmation ? (
                <>
                  {" "}
                  وبما أن هذا التعريف يتطلب تأكيداً سنوياً، فكل تطبيق يبدأ{" "}
                  <b>معلّقاً والرسم كامل</b> حتى يؤكِّد المدير المالي الأهلية لتلك السنة.
                </>
              ) : (
                <>
                  {" "}
                  وبما أن هذا التعريف لا يتطلب تأكيداً سنوياً، فالتطبيق يفعّل تلقائياً كل سنة —
                  راجع ذلك: منحة مفتوحة بلا مراجعة سنوية تبقى سارية بعد زوال سببها.
                </>
              )}
            </div>
          )}

          <label className="field">
            <span className="field__label">التبرير والمستندات</span>
            <input
              className="input"
              value={justification}
              onChange={(e) => setJustification(e.target.value)}
              placeholder="مثلاً: كتاب الموارد البشرية 1234 بتاريخ …"
            />
          </label>

          {refusal && (
            <div style={{ marginBottom: 12 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <Button
            variant="primary"
            size="lg"
            disabled={!can("discount.assign") || !ready}
            disabledReason={
              !can("discount.assign") ? reason("discount.assign") : !ready ? "أكمل الحقول" : undefined
            }
            busy={grant.isPending}
            onClick={() => grant.mutate()}
          >
            منح الخصم
          </Button>

          {/* Separation of duties, said before the refusal has to. */}
          <p className="note" style={{ marginTop: 10 }}>
            المانح لا يوافق على منحه. هذا المنح يُسجَّل الآن وينتظر توقيعاً ثانياً من صلاحية
            أخرى — وهو فصل واجبات لا نقص في صلاحيتك.
          </p>
        </Panel>

        {definitions.isSuccess && (definitions.data ?? []).length === 0 && (
          <EmptyState
            kind="not-yet"
            title="لا تعريفات خصم"
            detail="عرّف خصماً وانشر له إصداراً قبل منحه."
          />
        )}
      </div>
    </main>
  );
}
