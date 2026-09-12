import { useMemo, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { AccountView } from "@/api/types";
import { Money, MoneyField } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, Panel, Row } from "@/components/primitives";
import { amount, parseInput, type Amount } from "@/lib/money";

/**
 * A signed adjustment — §09.
 *
 * The frozen net never moves. Every later change to what is owed is one of
 * these rows, and what is owed is `net_snapshot + Σ adjustments`. So the sheet
 * shows the effective net **before and after, side by side**: an entry whose
 * consequence is not visible before it is written is an entry somebody gets
 * the sign wrong on.
 *
 * The type comes from a defined list rather than free text, because the
 * adjustment register is only readable if the reasons are countable.
 */
const TYPES = [
  { code: "correction", label: "تصحيح خطأ تسعير", hint: "سعر جُمِّد على أساس خاطئ" },
  { code: "transfer_in", label: "نقل رصيد وارد", hint: "من حساب مستبدَل" },
  { code: "transfer_out", label: "نقل رصيد صادر", hint: "إلى حساب بديل" },
  { code: "waiver", label: "إعفاء", hint: "بقرار إداري موثّق" },
  { code: "penalty", label: "غرامة", hint: "تأخّر أو مخالفة" },
  { code: "other", label: "أخرى", hint: "يتطلب سبباً مفصّلاً" },
];

export function AdjustSheet({
  account,
  onDone,
}: {
  account: AccountView;
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [type, setType] = useState(TYPES[0]!.code);
  const [direction, setDirection] = useState<"increase" | "decrease">("decrease");
  const [raw, setRaw] = useState("");
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [key] = useState(() => newIdempotencyKey());

  const parsed = parseInput(raw);
  const magnitude: Amount = parsed.ok ? parsed.value : (0n as Amount);
  const signed = (direction === "decrease" ? -magnitude : magnitude) as Amount;

  const before = amount(account.effective_net);
  // Display only — the server computes the authoritative figure. Showing it is
  // what stops an entry being written with the wrong sign.
  const after = useMemo(() => (before + signed) as Amount, [before, signed]);

  const post = useMutation({
    mutationFn: () =>
      api.post(
        "/accounts/adjustments",
        {
          account_id: account.id,
          type,
          amount: Number(signed),
          reason: reason.trim(),
        },
        { idempotencyKey: key },
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["account", account.id] });
      void queryClient.invalidateQueries({ queryKey: ["student-accounts"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const ready = parsed.ok && magnitude > 0n && reason.trim().length >= 3;

  return (
    <Panel title="قيد تسوية">
      <p className="note" style={{ marginBottom: 12 }}>
        الصافي المجمّد لا يتحرك. هذا القيد صف موقّع جديد، وما هو مطلوب = المجمّد + Σ القيود.
      </p>

      <label className="field">
        <span className="field__label">النوع</span>
        <select className="input" value={type} onChange={(e) => setType(e.target.value)}>
          {TYPES.map((t) => (
            <option key={t.code} value={t.code}>
              {t.label}
            </option>
          ))}
        </select>
        <span className="field__hint">{TYPES.find((t) => t.code === type)?.hint}</span>
      </label>

      <label className="field">
        <span className="field__label">الاتجاه</span>
        <div className="cluster">
          <button
            type="button"
            className={`btn${direction === "decrease" ? " btn--primary" : ""}`}
            onClick={() => setDirection("decrease")}
          >
            يُنقص المطلوب (−)
          </button>
          <button
            type="button"
            className={`btn${direction === "increase" ? " btn--primary" : ""}`}
            onClick={() => setDirection("increase")}
          >
            يزيد المطلوب (+)
          </button>
        </div>
      </label>

      <MoneyField label="المبلغ (دينار)" value={raw} onChange={setRaw} />

      <label className="field">
        <span className="field__label">السبب (إلزامي، ويُسجَّل باسمك)</span>
        <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
      </label>

      {/* Before and after, side by side. */}
      <div className="cols cols--half" style={{ marginBottom: 12 }}>
        <div className="stat">
          <b>
            <Money value={before} tone="plain" />
          </b>
          <span>الصافي الفعّال الآن</span>
        </div>
        <div className="stat">
          <b>
            <Money value={after} tone={after < 0n ? "auto" : "positive"} />
          </b>
          <span>بعد هذا القيد</span>
        </div>
      </div>

      {parsed.ok && magnitude > 0n && (
        <Row label="القيد">
          <Money value={signed} sign="always" />
        </Row>
      )}

      {after < 0n && (
        <div className="refusal refusal--pending" style={{ marginBottom: 12 }}>
          <p className="refusal__title">سيصبح الصافي الفعّال سالباً</p>
          <p className="refusal__body" style={{ margin: 0 }}>
            صافٍ سالب يعني أن الجامعة مدينة للطالب. هذا مشروع أحياناً، وغالباً علامة على إشارة
            مقلوبة — راجع الاتجاه قبل التنفيذ.
          </p>
        </div>
      )}

      {refusal && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!ready}
          disabledReason={
            !parsed.ok || magnitude === 0n
              ? "أدخل مبلغاً"
              : reason.trim().length < 3
                ? "اكتب سبباً"
                : undefined
          }
          busy={post.isPending}
          onClick={() => post.mutate()}
        >
          ترحيل القيد
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
