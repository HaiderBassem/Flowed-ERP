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
 * The official A5 copy is the server's PDF, opened in the browser's viewer.
 * It cannot be shown inline: the interface is served under `style-src 'self'`,
 * so an embedded copy would lose the document's own styling and preview
 * something other than the paper — the one thing this component exists to
 * prevent. A PDF also settles what an HTML popup left to the machine: the
 * font is embedded, the Arabic is shaped by the renderer rather than by
 * whatever face the desk happens to have, and the page size is the
 * document's rather than the print dialog's.
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

  // The official receipt is the server's PDF, opened in the browser's own
  // viewer. It carries the embedded font, the shaped Arabic and the A5 page
  // the paper actually needs, so it prints the same from any desk. The earlier
  const printText = () => {
    // The thermal roll is printed from the page rather than as a PDF, because
    // its content is the server's 42-column text and a PDF would impose a page
    // size the roll does not have. The class isolates the receipt element for
    // the duration, so the surrounding interface does not reach the paper.
    document.body.classList.add("printing-receipt");
    window.print();
    document.body.classList.remove("printing-receipt");
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
          معاينة حرارية
        </button>
        <button
          type="button"
          className="sheet__stage"
          data-state={format === "html" ? "current" : undefined}
          style={{ border: 0, background: "transparent", cursor: "pointer" }}
          onClick={() => setFormat("html")}
        >
          معاينة رسمية
        </button>
        <span className="grow" />
        {/*
          Two documents, two buttons. The formal one is A4 and in colour — the
          copy that is signed, filed and presented; the thermal one is 80mm of
          roll, black on white, cut where the content ends. Both are PDFs, so
          they print the same from any machine, which is the whole reason they
          stopped being browser pages.
        */}
        <Button onClick={() => void api.download(`/${kind}/${id}/receipt`, { format: "thermal" })}>
          وصل حراري 80mm
        </Button>
        <Button
          variant="primary"
          onClick={() => void api.download(`/${kind}/${id}/receipt`, { format: "pdf" })}
        >
          وصل رسمي A4
        </Button>
      </div>

      {format === "text" ? (
        <pre
          className="receipt receipt--80mm"
          style={{ whiteSpace: "pre-wrap", margin: 0 }}
        >
          {text.data}
        </pre>
      ) : (
        <div className="callout callout--note no-print">
          <p style={{ margin: 0 }}>
            النسخة الرسمية A5 تُفتح في نافذة مستقلة لأنها وثيقة الخادم بتنسيقها
            — المعاينة هنا ستفقد تنسيقها وتخالف الورق.
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
