import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { DiscountDefinitionView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";

const CATEGORIES = [
  { code: "social", label: "اجتماعي" },
  { code: "staff", label: "أبناء التدريسيين والموظفين" },
  { code: "merit", label: "تفوّق" },
  { code: "exemption", label: "إعفاء" },
  { code: "sibling", label: "أشقاء" },
  { code: "martyr", label: "ذوي الشهداء" },
  { code: "other", label: "أخرى" },
];

/** Discount definitions — §09. */
export function DiscountsScreen() {
  const { can, reason } = useSession();
  const [creating, setCreating] = useState(false);

  const definitions = useQuery({
    queryKey: ["discount-definitions"],
    queryFn: () => api.get<DiscountDefinitionView[]>("/discounts/definitions"),
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">تعريفات الخصومات</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("config.write")}
          disabledReason={reason("config.write")}
          onClick={() => setCreating((c) => !c)}
        >
          {creating ? "إغلاق" : "تعريف جديد"}
        </Button>
      </div>

      {creating && <DefinitionForm onDone={() => setCreating(false)} />}

      <div style={{ marginTop: 12 }}>
        <Panel title="التعريفات" flush>
          {definitions.isLoading && <Skeleton height={140} />}
          {definitions.isError &&
            (isRefusal(definitions.error) ? (
              <RefusalPanel
                refusal={definitions.error}
                onRetry={() => void definitions.refetch()}
              />
            ) : (
              <EmptyState kind="no-results" title="تعذّر جلب التعريفات" />
            ))}
          {definitions.isSuccess && (definitions.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا تعريفات خصم" />
          )}
          {(definitions.data ?? []).length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>الرمز</th>
                  <th>الاسم</th>
                  <th>الفئة</th>
                  <th>إعفاء كامل</th>
                  <th>تأكيد سنوي</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {(definitions.data ?? []).map((definition) => (
                  <tr key={definition.id}>
                    <td className="k">
                      <Link to={`/config/discounts/${definition.id}`}>{definition.code}</Link>
                    </td>
                    <td>
                      <Link to={`/config/discounts/${definition.id}`}>{definition.name_ar}</Link>
                    </td>
                    <td>
                      {CATEGORIES.find((c) => c.code === definition.category)?.label ??
                        definition.category}
                    </td>
                    <td>{definition.is_full_exemption ? "نعم" : "لا"}</td>
                    <td>
                      {definition.annual_reconfirmation ? (
                        <Chip
                          tone="pending"
                          hint="كل سنة جديدة يتولّد لها تطبيق ينتظر تأكيد الأهلية، والرسم كامل حتى يُؤكَّد"
                        >
                          مطلوب
                        </Chip>
                      ) : (
                        <span className="label">لا</span>
                      )}
                    </td>
                    <td>
                      {definition.is_active ? (
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
    </main>
  );
}

function DefinitionForm({ onDone }: { onDone: () => void }) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [category, setCategory] = useState("social");
  const [fullExemption, setFullExemption] = useState(false);
  const [annual, setAnnual] = useState(true);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post("/discounts/definitions", {
        code: code.trim(),
        name_ar: name.trim(),
        category,
        is_full_exemption: fullExemption,
        annual_reconfirmation: annual,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["discount-definitions"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="تعريف خصم جديد">
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

      <label className="field">
        <span className="field__label">الفئة</span>
        <select className="input" value={category} onChange={(e) => setCategory(e.target.value)}>
          {CATEGORIES.map((c) => (
            <option key={c.code} value={c.code}>
              {c.label}
            </option>
          ))}
        </select>
      </label>

      <label className="field" style={{ display: "flex", gap: 8, alignItems: "flex-start" }}>
        <input
          type="checkbox"
          checked={fullExemption}
          onChange={(e) => setFullExemption(e.target.checked)}
          style={{ marginTop: 4 }}
        />
        <span>
          <span className="field__label" style={{ marginBottom: 2 }}>
            إعفاء كامل
          </span>
          <span className="field__hint">
            يعفي الأساس القابل للخصم بالكامل — الرسوم غير القابلة للخصم تبقى مستحقة.
          </span>
        </span>
      </label>

      <label className="field" style={{ display: "flex", gap: 8, alignItems: "flex-start" }}>
        <input
          type="checkbox"
          checked={annual}
          onChange={(e) => setAnnual(e.target.checked)}
          style={{ marginTop: 4 }}
        />
        <span>
          <span className="field__label" style={{ marginBottom: 2 }}>
            يتطلب تأكيد أهلية سنوياً
          </span>
          {/* This flag is what makes an "all years" grant safe. */}
          <span className="field__hint">
            مع منحٍ يغطي «كل السنوات»، هذا العلم هو ما يجعل المنح آمناً: كل سنة جديدة يتولّد لها
            تطبيق معلّق، والرسم يبقى كاملاً حتى يؤكَّد.
          </span>
        </span>
      </label>

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
          حفظ التعريف
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
        <span className="grow" />
        <span className="note">القيمة تُحدَّد في إصدار، لا في التعريف.</span>
      </div>
    </Panel>
  );
}
