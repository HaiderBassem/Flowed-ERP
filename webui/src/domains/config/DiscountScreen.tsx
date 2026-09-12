import { useState } from "react";
import { useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { DiscountDetailView, DiscountVersionView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDateTime } from "@/lib/dates";
import { labelDiscountCategory } from "@/design/lexicon";

/**
 * One discount definition and its versions — §09.
 *
 * There is no edit control on a published version, and its absence is the
 * design: accounts carry a foreign key to the exact version that priced them,
 * so editing one would silently restate money that receipts already reference.
 * In its place sits "new version", and the old one stays bound to what it
 * priced.
 */
export function DiscountScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();
  const [adding, setAdding] = useState(false);

  const detail = useQuery({
    queryKey: ["discount-definition", id],
    queryFn: () => api.get<DiscountDetailView>(`/discounts/definitions/${id}`),
    enabled: Boolean(id),
  });

  if (detail.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  if (detail.isError) {
    return (
      <main className="screen">
        {isRefusal(detail.error) ? (
          <RefusalPanel refusal={detail.error} onRetry={() => void detail.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح التعريف" />
        )}
      </main>
    );
  }

  const definition = detail.data!.definition;
  const published = detail.data!.published_version;

  return (
    <main className="screen">
      <Crumbs
        items={[{ label: "تعريفات الخصومات", to: "/config/discounts" }, { label: definition.name_ar }]}
      />
      <div className="screen__head">
        <h1 className="screen__title">{definition.name_ar}</h1>
        <span className="k">{definition.code}</span>
        {definition.annual_reconfirmation && (
          <Chip tone="pending" hint="كل سنة جديدة تنتظر تأكيد أهلية">
            تأكيد سنوي
          </Chip>
        )}
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("config.write")}
          disabledReason={reason("config.write")}
          onClick={() => setAdding((a) => !a)}
        >
          {adding ? "إغلاق" : "إصدار جديد"}
        </Button>
      </div>

      {adding && <VersionForm definitionId={definition.id} onDone={() => setAdding(false)} />}

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <Panel title="التعريف">
          <Row label="الرمز">
            <span className="k">{definition.code}</span>
          </Row>
          <Row label="الفئة">{labelDiscountCategory(definition.category)}</Row>
          <Row label="إعفاء كامل">{definition.is_full_exemption ? "نعم" : "لا"}</Row>
          <Row label="تأكيد سنوي">{definition.annual_reconfirmation ? "مطلوب" : "لا"}</Row>
          <Row label="الحالة">
            {definition.is_active ? <Chip tone="live">فعّال</Chip> : <Chip tone="muted">معطّل</Chip>}
          </Row>
        </Panel>

        <Panel title="الإصدار المنشور">
          {published ? (
            <VersionBody version={published} />
          ) : (
            <EmptyState
              kind="not-yet"
              title="لا إصدار منشور"
              detail="التعريف بلا إصدار منشور لا يُطبَّق على أي حساب."
            />
          )}
        </Panel>
      </div>

      <div style={{ marginTop: 12 }}>
        <Panel title="خط الإصدارات">
          {published ? (
            <ul className="timeline">
              <li className="timeline__item">
                <span className="timeline__dot" data-tone="live" />
                <span className="timeline__when">
                  {formatDateTime(published.published_at ?? null)}
                </span>{" "}
                <b>الإصدار {published.version_no}</b>{" "}
                <span className="timeline__who">
                  — نُشر{published.published_by ? ` بواسطة ${published.published_by}` : ""}
                </span>
                <div className="note">
                  الحسابات التي سُعِّرت تحته تشير إليه بالمفتاح، ولذلك لا يُعدَّل.
                </div>
              </li>
            </ul>
          ) : (
            <p className="note">لا إصدارات منشورة بعد.</p>
          )}
          {/* The API returns only the published version on this route; the
              draft history is not enumerable, and saying so beats an empty
              list that implies there is none. */}
          <p className="note" style={{ marginTop: 8 }}>
            يعيد الـ API الإصدار المنشور فقط على هذا المسار، فالمسودات السابقة غير قابلة للسرد
            هنا.
          </p>
        </Panel>
      </div>
    </main>
  );
}

function VersionBody({ version }: { version: DiscountVersionView }) {
  return (
    <>
      <Row label="رقم الإصدار">
        <span className="num">{version.version_no}</span>
      </Row>
      <Row label="نوع القيمة">
        {version.value_type === "percentage" ? "نسبة مئوية" : "مبلغ ثابت"}
      </Row>
      {version.value_type === "percentage" ? (
        <Row label="النسبة">
          <span className="num">{version.rate_percent ?? (version.rate_bp ?? 0) / 100}٪</span>
        </Row>
      ) : (
        <Row label="المبلغ">
          <Money value={version.fixed_amount ?? 0} tone="plain" />
        </Row>
      )}
      {version.per_application_cap && (
        <Row label="سقف لكل تطبيق">
          <Money value={version.per_application_cap} tone="plain" />
        </Row>
      )}
      <Row label="قابل للتراكم">{version.stackable ? "نعم" : "لا"}</Row>
      <Row label="الأولوية">
        <span className="num">{version.priority}</span>
      </Row>
      <Row label="يتطلب موافقة">
        {version.requires_approval ? version.approval_role ?? "نعم" : "لا"}
      </Row>
      {version.applies_to_components && version.applies_to_components.length > 0 && (
        <Row label="ينطبق على">
          <span className="label ltr">{version.applies_to_components.join(", ")}</span>
        </Row>
      )}
    </>
  );
}

function VersionForm({ definitionId, onDone }: { definitionId: string; onDone: () => void }) {
  const queryClient = useQueryClient();
  const [valueType, setValueType] = useState<"percentage" | "fixed">("percentage");
  const [percent, setPercent] = useState("20");
  const [fixed, setFixed] = useState("");
  const [stackable, setStackable] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post(`/discounts/definitions/${definitionId}/versions`, {
        value_type: valueType,
        // Basis points on the wire: half-up integer rounding is what makes a
        // discount recomputed in five years bit-identical.
        ...(valueType === "percentage"
          ? { rate_bp: Math.round(Number(percent) * 100) }
          : { fixed_amount: Number(fixed.replace(/,/g, "")) }),
        stackable,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["discount-definition", definitionId] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="إصدار جديد">
      <p className="note" style={{ marginBottom: 10 }}>
        يُحفظ مسودةً. النشر خطوة منفصلة، وبعده لا يُعدَّل — الحسابات ستشير إليه.
      </p>

      <label className="field">
        <span className="field__label">نوع القيمة</span>
        <div className="cluster">
          <button
            type="button"
            className={`btn${valueType === "percentage" ? " btn--primary" : ""}`}
            onClick={() => setValueType("percentage")}
          >
            نسبة مئوية
          </button>
          <button
            type="button"
            className={`btn${valueType === "fixed" ? " btn--primary" : ""}`}
            onClick={() => setValueType("fixed")}
          >
            مبلغ ثابت
          </button>
        </div>
      </label>

      {valueType === "percentage" ? (
        <label className="field">
          <span className="field__label">النسبة ٪</span>
          <input
            className="input"
            inputMode="decimal"
            dir="ltr"
            value={percent}
            onChange={(e) => setPercent(e.target.value)}
          />
          <span className="field__hint">
            تُخزَّن كنقاط أساس (<span className="num">{Math.round(Number(percent) * 100) || 0}</span>{" "}
            نقطة) بتقريب نصفي لأعلى، فتبقى قابلة لإعادة الحساب بالضبط بعد سنوات.
          </span>
        </label>
      ) : (
        <label className="field">
          <span className="field__label">المبلغ الثابت (دينار)</span>
          <input
            className="input"
            inputMode="numeric"
            dir="ltr"
            value={fixed}
            onChange={(e) => setFixed(e.target.value)}
          />
        </label>
      )}

      <label className="field" style={{ display: "flex", gap: 8, alignItems: "center" }}>
        <input
          type="checkbox"
          checked={stackable}
          onChange={(e) => setStackable(e.target.checked)}
        />
        <span className="field__label" style={{ margin: 0 }}>
          قابل للتراكم مع خصومات أخرى
        </span>
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button variant="primary" busy={create.isPending} onClick={() => create.mutate()}>
          حفظ الإصدار
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
