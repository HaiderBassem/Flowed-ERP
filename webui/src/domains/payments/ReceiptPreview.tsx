import { useState } from "react";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal } from "@/api/errors";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Skeleton } from "@/components/primitives";

/**
 * ReceiptPreview — §07.
 *
 * The preview must match the output, so it *is* the output: both formats are
 * fetched from the server, which owns the receipt's layout and its Arabic
 * spelling of the amount. Re-drawing the slip from the payment fields in React
 * would produce a second receipt design that drifts from the printed one, and
 * the printed one is the document the university accepts.
 *
 * The 80mm text rendering is the one the window actually feeds to its roll
 * printer, so it is the default tab and it prints from this page directly.
 *
 * The A5 HTML opens in its own window. It cannot be shown inline: the server's
 * receipt carries a <style> block, and the interface is served under
 * `style-src 'self'`, which would strip it and show a preview that does not
 * match the paper — the one thing this component exists to prevent.
 */
export function ReceiptPreview({
  kind,
  id,
  autoPrint,
}: {
  kind: "payments" | "refunds";
  id: string;
  autoPrint?: boolean;
}) {
  const [format, setFormat] = useState<"text" | "html">("text");

  const text = useQuery({
    queryKey: ["receipt", kind, id, "text"],
    queryFn: () => api.document(`/${kind}/${id}/receipt`, { format: "text" }),
    enabled: Boolean(id),
  });

  const openA5 = async () => {
    const html = await api.document(`/${kind}/${id}/receipt`, { format: "html" });
    const window_ = window.open("", "_blank", "width=760,height=900");
    if (!window_) return;
    window_.document.open();
    window_.document.write(html);
    window_.document.close();
    window_.focus();
  };

  const printText = () => {
    // Printed from a dedicated element so the surrounding interface does not
    // reach the roll. print.css hides everything else.
    window.print();
  };

  if (text.isLoading) return <Skeleton height={220} />;

  if (text.isError) {
    return isRefusal(text.error) ? (
      <RefusalPanel refusal={text.error} onRetry={() => void text.refetch()} />
    ) : (
      <EmptyState kind="no-results" title="تعذّر تحضير الوصل" />
    );
  }

  return (
    <div>
      <div className="cluster no-print" style={{ marginBottom: 10 }}>
        <button
          type="button"
          className="sheet__stage"
          data-state={format === "text" ? "current" : undefined}
          style={{ border: 0, background: "transparent", cursor: "pointer" }}
          onClick={() => setFormat("text")}
        >
          حراري 80mm
        </button>
        <button
          type="button"
          className="sheet__stage"
          data-state={format === "html" ? "current" : undefined}
          style={{ border: 0, background: "transparent", cursor: "pointer" }}
          onClick={() => setFormat("html")}
        >
          رسمي A5
        </button>
        <span className="grow" />
        {format === "text" ? (
          <Button variant="primary" onClick={printText}>
            طباعة
          </Button>
        ) : (
          <Button variant="primary" onClick={() => void openA5()}>
            فتح وطباعة A5
          </Button>
        )}
      </div>

      {format === "text" ? (
        <pre className="receipt receipt--80mm" style={{ whiteSpace: "pre-wrap", margin: 0 }}>
          {text.data}
        </pre>
      ) : (
        <div className="callout callout--note no-print">
          <p style={{ margin: 0 }}>
            النسخة الرسمية A5 تُفتح في نافذة مستقلة لأنها وثيقة الخادم بتنسيقها — المعاينة هنا
            ستفقد تنسيقها وتخالف الورق.
          </p>
        </div>
      )}

      {autoPrint && <AutoPrint onPrint={printText} />}
    </div>
  );
}

function AutoPrint({ onPrint }: { onPrint: () => void }) {
  const [done, setDone] = useState(false);
  if (!done) {
    setDone(true);
    window.setTimeout(onPrint, 250);
  }
  return null;
}
