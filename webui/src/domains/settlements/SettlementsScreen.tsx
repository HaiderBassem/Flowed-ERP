import { useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type {
  Page,
  SettlementBatchDetailView,
  SettlementBatchView,
  SettlementExceptionView,
  SettlementImportView,
  SettlementLineView,
  UnconfirmedPaymentView,
} from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDate, formatDateTime } from "@/lib/dates";
import { amount } from "@/lib/money";

type Tab = "batches" | "exceptions" | "unconfirmed";

/**
 * Bank settlement — the non-cash mirror of the drawer count.
 *
 * A bank statement is imported, its lines auto-match against posted payments
 * by reference and amount, and what remains is worked by a person: the
 * exceptions queue. The other direction matters just as much and gets its own
 * tab — posted non-cash collections **no statement has confirmed**, which is
 * where a receipt written against money that never arrived would show.
 *
 * The healthy state of the exceptions queue is empty, and it is celebrated
 * as such (§09): zero here is the finding, not the absence of one.
 */
export function SettlementsScreen() {
  const [params, setParams] = useSearchParams();
  const tab = (params.get("tab") as Tab) ?? "batches";
  const { can, reason } = useSession();

  const setTab = (next: Tab) => {
    const p = new URLSearchParams(params);
    if (next === "batches") p.delete("tab");
    else p.set("tab", next);
    setParams(p, { replace: true });
  };

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">التسويات البنكية</h1>
      </div>

      <div className="cluster" style={{ marginBottom: 12 }}>
        {(
          [
            ["batches", "الكشوف المستوردة"],
            ["exceptions", "الاستثناءات"],
            ["unconfirmed", "مقبوض بلا تأكيد"],
          ] as const
        ).map(([id, label]) => (
          <button
            key={id}
            type="button"
            className="sheet__stage"
            data-state={tab === id ? "current" : undefined}
            style={{ border: 0, background: "transparent", cursor: "pointer" }}
            onClick={() => setTab(id)}
          >
            {label}
          </button>
        ))}
      </div>

      {tab === "batches" && <BatchesTab canWrite={can("settlement.write")} writeReason={reason("settlement.write")} />}
      {tab === "exceptions" && <ExceptionsTab canWrite={can("settlement.write")} writeReason={reason("settlement.write")} />}
      {tab === "unconfirmed" && <UnconfirmedTab />}
    </main>
  );
}

/* ------------------------------------------------------------------ batches */

function BatchesTab({
  canWrite,
  writeReason,
}: {
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const [sourceCode, setSourceCode] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [imported, setImported] = useState<SettlementImportView | null>(null);
  const [openBatch, setOpenBatch] = useState<string | null>(null);

  // The one paginated endpoint in this subsystem — the batch list carries the
  // {data, total, …} envelope; exceptions and unconfirmed are bare arrays.
  const batches = useQuery({
    queryKey: ["settlements", "batches"],
    queryFn: () => api.get<Page<SettlementBatchView>>("/settlements", { query: { limit: 50 } }),
  });

  const importStatement = useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      form.append("source_code", sourceCode.trim());
      return api.post<SettlementImportView>("/settlements/import", form);
    },
    onSuccess: (result) => {
      setImported(result);
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["settlements"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <>
      <Panel title="استيراد كشف">
        <p className="note" style={{ marginBottom: 10 }}>
          CSV من المصرف أو مزوّد الدفع. السطور تُطابَق آلياً بالمرجع والمبلغ؛ ما لم يُطابَق
          يذهب إلى الاستثناءات ليُحسم بيد إنسان — لا شيء يُحسم بصمت.
        </p>
        <div className="cluster">
          <label className="field" style={{ margin: 0, minWidth: 220 }}>
            <span className="field__label">رمز المصدر</span>
            <input
              className="input ltr"
              placeholder="RAFIDAIN / QICARD…"
              value={sourceCode}
              onChange={(e) => setSourceCode(e.target.value)}
            />
          </label>
          <input
            ref={fileRef}
            type="file"
            accept=".csv"
            style={{ display: "none" }}
            onChange={(e) => {
              const file = e.target.files?.[0];
              if (file) importStatement.mutate(file);
              e.target.value = "";
            }}
          />
          <Button
            variant="primary"
            disabled={!canWrite || !sourceCode.trim()}
            disabledReason={!canWrite ? writeReason : "سمِّ المصدر أولاً"}
            busy={importStatement.isPending}
            onClick={() => fileRef.current?.click()}
          >
            اختيار الملف واستيراده
          </Button>
        </div>
        {refusal && (
          <div style={{ marginTop: 10 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}
        {imported && (
          <div className="callout" style={{ marginTop: 10 }}>
            استُورد <b className="num">{imported.batch.line_count}</b> سطراً، طوبق منها{" "}
            <b className="num">{imported.batch.matched_count}</b> آلياً؛{" "}
            {imported.exceptions === 0 ? (
              <b>لا استثناءات — تسوية نظيفة.</b>
            ) : (
              <>
                <b className="num">{imported.exceptions}</b> استثناءً بانتظار الحسم.
              </>
            )}
          </div>
        )}
      </Panel>

      <div style={{ marginTop: 12 }}>
        <Panel title="الكشوف" flush>
          {batches.isLoading && <Skeleton height={120} />}
          {batches.isSuccess && (batches.data?.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا كشوف مستوردة بعد" />
          )}
          {(batches.data?.data ?? []).length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>المصدر</th>
                  <th>الملف</th>
                  <th>المدى</th>
                  <th className="n">سطور</th>
                  <th className="n">مطابَق</th>
                  <th className="n col-group-money">الإجمالي</th>
                  <th className="n">المطابَق مالياً</th>
                  <th />
                </tr>
              </thead>
              <tbody>
                {(batches.data?.data ?? []).map((batch) => (
                  <tr key={batch.id}>
                    <td className="k ltr">{batch.source_code}</td>
                    <td>{batch.filename}</td>
                    <td className="num">
                      {batch.statement_from ? formatDate(batch.statement_from) : "—"} →{" "}
                      {batch.statement_to ? formatDate(batch.statement_to) : "—"}
                    </td>
                    <td className="n">{batch.line_count}</td>
                    <td className="n">
                      {batch.matched_count}
                      {batch.matched_count < batch.line_count && (
                        <Chip tone="pending">{batch.line_count - batch.matched_count} معلّق</Chip>
                      )}
                    </td>
                    <td className="n col-group-money">
                      <Money value={batch.total_amount} tone="plain" />
                    </td>
                    <td className="n">
                      <Money value={batch.matched_amount} tone="positive" />
                    </td>
                    <td>
                      <Button size="sm" onClick={() => setOpenBatch(openBatch === batch.id ? null : batch.id)}>
                        {openBatch === batch.id ? "إغلاق" : "السطور"}
                      </Button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>

      {openBatch && <BatchDetail batchId={openBatch} canWrite={canWrite} writeReason={writeReason} />}
    </>
  );
}

function BatchDetail({
  batchId,
  canWrite,
  writeReason,
}: {
  batchId: string;
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const detail = useQuery({
    queryKey: ["settlements", "batch", batchId],
    queryFn: () => api.get<SettlementBatchDetailView>(`/settlements/${batchId}`),
  });

  const rematch = useMutation({
    mutationFn: () => api.post(`/settlements/${batchId}/rematch`, {}),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["settlements"] });
    },
  });

  if (detail.isLoading) return <Skeleton height={140} />;
  if (!detail.data) return null;

  return (
    <div style={{ marginTop: 12 }}>
      <Panel
        title={
          <>
            سطور <span className="k ltr">{detail.data.batch.filename}</span>
          </>
        }
        aside={
          <Button
            size="sm"
            disabled={!canWrite}
            disabledReason={writeReason}
            busy={rematch.isPending}
            onClick={() => rematch.mutate()}
            title="يعيد المطابقة الآلية بعد ترحيل دفعات جديدة"
          >
            إعادة المطابقة
          </Button>
        }
        flush
      >
        <LineTable lines={detail.data.lines} canWrite={canWrite} writeReason={writeReason} />
      </Panel>
    </div>
  );
}

function LineTable({
  lines,
  canWrite,
  writeReason,
}: {
  lines: SettlementLineView[];
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  if (lines.length === 0) return <EmptyState kind="not-yet" title="لا سطور" />;
  return (
    <table className="grid">
      <thead>
        <tr>
          <th className="n">#</th>
          <th>المرجع</th>
          <th className="n col-group-money">المبلغ</th>
          <th className="n">الفرق</th>
          <th>التاريخ</th>
          <th>الحالة</th>
          <th />
        </tr>
      </thead>
      <tbody>
        {lines.map((line) => (
          <SettlementLineRow
            key={line.id}
            line={line}
            canWrite={canWrite}
            writeReason={writeReason}
          />
        ))}
      </tbody>
    </table>
  );
}

function SettlementLineRow({
  line,
  canWrite,
  writeReason,
}: {
  line: SettlementLineView;
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const [resolving, setResolving] = useState(false);
  const [paymentId, setPaymentId] = useState("");
  const [note, setNote] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const resolve = useMutation({
    mutationFn: (status: "matched" | "ignored") =>
      api.post(`/settlements/lines/${line.id}/resolve`, {
        status,
        ...(status === "matched" ? { payment_id: paymentId.trim() } : {}),
        note: note.trim(),
      }),
    onSuccess: () => {
      setResolving(false);
      void queryClient.invalidateQueries({ queryKey: ["settlements"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const settled = line.match_status === "matched" || line.match_status === "ignored";

  return (
    <>
      <tr>
        <td className="n">{line.line_no}</td>
        <td className="k ltr">{line.external_ref}</td>
        <td className="n col-group-money">
          <Money value={line.amount} tone="plain" />
        </td>
        <td className="n">
          {amount(line.variance) === 0n ? (
            <span className="label">—</span>
          ) : (
            <Money value={line.variance} sign="always" />
          )}
        </td>
        <td className="num">{line.value_date ? formatDate(line.value_date) : "—"}</td>
        <td>
          <LineStatus status={line.match_status} />
          {line.review_note && <span className="label"> — {line.review_note}</span>}
        </td>
        <td>
          {!settled && (
            <Button
              size="sm"
              disabled={!canWrite}
              disabledReason={writeReason}
              onClick={() => setResolving((r) => !r)}
            >
              حسم
            </Button>
          )}
        </td>
      </tr>
      {resolving && (
        <tr>
          <td colSpan={7} style={{ background: "var(--surface-2)" }}>
            <div className="cluster" style={{ padding: "6px 0" }}>
              <input
                className="input ltr"
                style={{ maxWidth: 320 }}
                placeholder="معرّف الدفعة للمطابقة اليدوية"
                value={paymentId}
                onChange={(e) => setPaymentId(e.target.value)}
              />
              <input
                className="input"
                style={{ maxWidth: 280 }}
                placeholder="ملاحظة"
                value={note}
                onChange={(e) => setNote(e.target.value)}
              />
              <Button
                size="sm"
                variant="primary"
                disabled={!paymentId.trim()}
                disabledReason='"مطابَق" بلا دفعة لا يُفرَّق عن تصفية القائمة'
                busy={resolve.isPending}
                onClick={() => resolve.mutate("matched")}
              >
                مطابقة يدوية
              </Button>
              <Button
                size="sm"
                variant="danger"
                busy={resolve.isPending}
                onClick={() => resolve.mutate("ignored")}
              >
                تجاهل بملاحظة
              </Button>
            </div>
            {refusal && <RefusalPanel refusal={refusal} />}
          </td>
        </tr>
      )}
    </>
  );
}

function LineStatus({ status }: { status: string }) {
  switch (status) {
    case "matched":
      return <Chip tone="live">مطابَق</Chip>;
    case "unmatched":
      return <Chip tone="pending">بلا مطابقة</Chip>;
    case "duplicate":
      return <Chip tone="void">مكرر</Chip>;
    case "amount_mismatch":
      return <Chip tone="void">فرق مبلغ</Chip>;
    case "ignored":
      return <Chip tone="muted">متجاهَل</Chip>;
    default:
      return <Chip tone="muted">{status}</Chip>;
  }
}

/* --------------------------------------------------------------- exceptions */

function ExceptionsTab({
  canWrite,
  writeReason,
}: {
  canWrite: boolean;
  writeReason: string | undefined;
}) {
  const exceptions = useQuery({
    queryKey: ["settlements", "exceptions"],
    queryFn: () => api.get<SettlementExceptionView[]>("/settlements/exceptions"),
    refetchInterval: 120_000,
  });

  const rows = exceptions.data ?? [];

  if (exceptions.isLoading) return <Skeleton height={140} />;

  if (rows.length === 0) {
    return (
      <Panel>
        <EmptyState
          kind="clean"
          title="لا استثناءات — كل سطر في الكشوف وجد دفعته"
          detail="الفراغ هنا هو النتيجة المطلوبة: تسوية بلا بقايا."
        />
      </Panel>
    );
  }

  return (
    <>
      <div className="refusal refusal--pending" style={{ marginBottom: 12 }}>
        <p className="refusal__title">{rows.length} سطراً بانتظار حسم بشري</p>
        <p className="refusal__body" style={{ margin: 0 }}>
          سطر بلا مطابقة إما دفعة لم تُرحَّل، أو مرجع كُتب خطأ، أو مال وصل المصرف ولم يصل
          الدفاتر — وكل واحدة من الثلاث قصة مختلفة تماماً.
        </p>
      </div>
      <Panel title="الاستثناءات" flush>
        <table className="grid">
          <thead>
            <tr>
              <th>المصدر</th>
              <th className="n">سطر</th>
              <th>المرجع</th>
              <th className="n col-group-money">المبلغ</th>
              <th className="n">الفرق</th>
              <th>الحالة</th>
              <th>الدفعة</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.line_id}>
                <td className="k ltr">{row.source_code}</td>
                <td className="n">{row.line_no}</td>
                <td className="k ltr">{row.external_ref ?? "—"}</td>
                <td className="n col-group-money">
                  <Money value={row.amount} tone="plain" />
                </td>
                <td className="n">
                  {amount(row.variance) === 0n ? (
                    <span className="label">—</span>
                  ) : (
                    <Money value={row.variance} sign="always" />
                  )}
                </td>
                <td>
                  <LineStatus status={row.match_status} />
                </td>
                <td>
                  {row.payment_id ? (
                    <Link className="k ltr" to={`/payments/${row.payment_id}`}>
                      فتح الوصل
                    </Link>
                  ) : (
                    <span className="label">—</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>
      <p className="note" style={{ marginTop: 8 }}>
        الحسم يجري من كشف المصدر نفسه — افتح الكشف من لسان «الكشوف المستوردة» واحسم سطوره
        هناك، مطابقةً يدوية بدفعة مسماة أو تجاهلاً بملاحظة.
        {!canWrite && ` ${writeReason ?? ""}`}
      </p>
    </>
  );
}

/* -------------------------------------------------------------- unconfirmed */

function UnconfirmedTab() {
  const unconfirmed = useQuery({
    queryKey: ["settlements", "unconfirmed"],
    queryFn: () => api.get<UnconfirmedPaymentView[]>("/settlements/unconfirmed"),
  });

  const rows = unconfirmed.data ?? [];

  if (unconfirmed.isLoading) return <Skeleton height={140} />;

  if (rows.length === 0) {
    return (
      <Panel>
        <EmptyState
          kind="clean"
          title="كل مقبوض غير نقدي أكّده كشف"
          detail="هذا اللسان هو المرآة المعاكسة: وصل كُتب على حوالة لم تصل يظهر هنا."
        />
      </Panel>
    );
  }

  return (
    <>
      <div className="refusal refusal--pending" style={{ marginBottom: 12 }}>
        <p className="refusal__title">{rows.length} مقبوضاً غير نقدي لم يؤكده أي كشف</p>
        <p className="refusal__body" style={{ margin: 0 }}>
          وصل على حوالة لم يؤكدها المصرف إما كشف لم يُستورد بعد، أو مال لم يصل فعلاً. الأول
          ينتظر؛ الثاني هو ما يوجد هذا اللسان لكشفه.
        </p>
      </div>
      <Panel title="مقبوض بلا تأكيد" flush>
        <table className="grid">
          <thead>
            <tr>
              <th>الوصل</th>
              <th>الطريقة</th>
              <th>المرجع</th>
              <th className="n col-group-money">المبلغ</th>
              <th>قُبض</th>
            </tr>
          </thead>
          <tbody>
            {rows.map((row) => (
              <tr key={row.payment_id}>
                <td>
                  <Link className="k ltr" to={`/payments/${row.payment_id}`}>
                    {row.receipt_no ?? "فتح الوصل"}
                  </Link>
                </td>
                <td className="k ltr">{row.method_code}</td>
                <td className="k ltr">{row.method_reference ?? "—"}</td>
                <td className="n col-group-money">
                  <Money value={row.amount} tone="plain" />
                </td>
                <td className="num">{formatDateTime(row.paid_at)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      </Panel>
    </>
  );
}
