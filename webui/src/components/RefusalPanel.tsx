import { useState } from "react";

import { Refusal } from "@/api/errors";

export interface RefusalAction {
  label: string;
  onClick: () => void;
  /** The "instead" path — the one that turns a refusal into a route forward. */
  primary?: boolean;
}

/**
 * The refusal panel — §07 pattern three, and the visual treatment §10 assigns
 * to each kind of fault.
 *
 * Three fixed parts: what was prevented, why, and what to do instead. The last
 * one is what separates a system that is obeyed from a system that is routed
 * around: "not allowed" ends the conversation and starts a phone call.
 *
 * The code and the request id stay visible in small type and copy on click.
 * That is the difference between a thirty-second support call and a
 * twenty-minute one.
 */
export function RefusalPanel({
  refusal,
  actions = [],
  onRetry,
}: {
  refusal: Refusal;
  actions?: RefusalAction[];
  /** Offered for the cases where retrying is the correct move. */
  onRetry?: () => void;
}) {
  const [copied, setCopied] = useState<string | null>(null);

  const copy = (text: string) => {
    void navigator.clipboard?.writeText(text).then(
      () => {
        setCopied(text);
        window.setTimeout(() => setCopied(null), 1500);
      },
      () => undefined,
    );
  };

  const tone = toneFor(refusal);

  return (
    <div
      className={`refusal${tone === "pending" ? " refusal--pending" : ""}${
        refusal.kind === "invariant_violation" ? " refusal--invariant" : ""
      }`}
      role="alert"
    >
      <p className="refusal__title">{titleFor(refusal)}</p>
      <p className="refusal__body">{refusal.message}</p>

      {refusal.remedy && <p className="refusal__remedy">{refusal.remedy}</p>}
      {!refusal.remedy && guidance(refusal) && (
        <p className="refusal__remedy">{guidance(refusal)}</p>
      )}

      <DetailList refusal={refusal} />

      {(actions.length > 0 || onRetry) && (
        <div className="refusal__actions">
          {onRetry && (
            <button type="button" className="btn btn--primary" onClick={onRetry}>
              {refusal.outcomeUnknown ? "أعد المحاولة بنفس المفتاح" : "أعد المحاولة"}
            </button>
          )}
          {actions.map((action) => (
            <button
              key={action.label}
              type="button"
              className={`btn${action.primary ? " btn--primary" : ""}`}
              onClick={action.onClick}
            >
              {action.label}
            </button>
          ))}
        </div>
      )}

      {/* The reference line, labelled in the operator's language. The values
          stay verbatim and copyable — they are what turn a twenty-minute
          support call into thirty seconds — but they are presented as
          references, not as leaked internals. */}
      <div className="refusal__meta">
        <span>رمز الرفض:</span>
        <code
          onClick={() => copy(refusal.code)}
          title="انقر للنسخ"
          role="button"
          tabIndex={0}
          onKeyDown={(e) => e.key === "Enter" && copy(refusal.code)}
        >
          {refusal.code}
        </code>
        {refusal.requestId && (
          <>
            <span>المرجع:</span>
            <code
              className="ltr"
              onClick={() => copy(refusal.requestId!)}
              title="انقر للنسخ"
              role="button"
              tabIndex={0}
              onKeyDown={(e) => e.key === "Enter" && copy(refusal.requestId!)}
            >
              {refusal.requestId}
            </code>
          </>
        )}
        {copied && <span style={{ color: "var(--accent)" }}>نُسخ</span>}
      </div>
    </div>
  );
}

/**
 * Structured context the server sent, minus the remedy already shown above.
 *
 * Folded behind a disclosure: the field names are wire names, which is
 * exactly what a support call needs verbatim and exactly what a cashier
 * mid-queue does not — so they exist one click away instead of in the
 * operator's face.
 */
function DetailList({ refusal }: { refusal: Refusal }) {
  const entries = Object.entries(refusal.details).filter(
    ([key, value]) => key !== "remedy" && value !== null && value !== undefined,
  );
  if (entries.length === 0) return null;
  return (
    <details className="refusal__details">
      <summary className="note" style={{ cursor: "pointer" }}>
        تفاصيل للدعم الفني
      </summary>
      <dl className="note" style={{ display: "grid", gridTemplateColumns: "auto 1fr", gap: "2px 10px", margin: "6px 0 8px" }}>
        {entries.map(([key, value]) => (
          <div key={key} style={{ display: "contents" }}>
            <dt style={{ fontFamily: "var(--mono)" }}>{key}</dt>
            <dd style={{ margin: 0 }}>{String(value)}</dd>
          </div>
        ))}
      </dl>
    </details>
  );
}

function toneFor(refusal: Refusal): "void" | "pending" {
  // forbidden is separation of duties working as designed, not a technical
  // fault — §10 asks it not to look like one.
  return refusal.kind === "forbidden" || refusal.kind === "rate_limited" ? "pending" : "void";
}

function titleFor(refusal: Refusal): string {
  switch (refusal.kind) {
    case "precondition_failed":
      return "الحالة الحالية تمنع هذا الأمر";
    case "forbidden":
      return "هذا الإجراء يخصّ دوراً آخر";
    case "conflict":
      return "تعارض — غيّر شخص آخر هذا الصف";
    case "not_found":
      return "غير موجود";
    case "unauthorized":
      return "انتهت الجلسة";
    case "rate_limited":
      return "تجاوزت حد الطلبات";
    case "invariant_violation":
      return "النظام أمسك نفسه قبل كتابة حالة فاسدة";
    case "unreachable":
      return "لم يصلنا جواب";
    case "internal":
      return "عطل في الخادم";
    default:
      return "طلب مرفوض";
  }
}

/** §10's column of "why this treatment", as a sentence for the operator. */
function guidance(refusal: Refusal): string | null {
  switch (refusal.kind) {
    case "conflict":
      return "الحل قراءة لا إعادة إرسال: أعد تحميل الصف وقارن الفرق قبل المحاولة ثانية.";
    case "forbidden":
      return "هذا فصل واجبات يعمل كما صُمِّم. اطلب الإجراء وسيصل إلى من يملكه.";
    case "invariant_violation":
      return "أبلغ الدعم بالرمز ورقم الطلب أدناه. لا تعد المحاولة.";
    case "rate_limited":
      return refusal.retryAfter
        ? `أعد المحاولة بعد ${refusal.retryAfter} ثانية — ما كتبته محفوظ.`
        : "أعد المحاولة بعد قليل — ما كتبته محفوظ.";
    case "internal":
      return "أبلغ الدعم برقم الطلب أدناه.";
    default:
      return null;
  }
}
