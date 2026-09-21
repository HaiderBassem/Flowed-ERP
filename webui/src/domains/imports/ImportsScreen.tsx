import { useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { ImportBatchView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { elapsed, formatDateTime } from "@/lib/dates";

/** Import batches — the list, and the upload that starts one. */
export function ImportsScreen() {
  const { can, reason } = useSession();
  const { activeYear } = useWorkingContext();
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const fileRef = useRef<HTMLInputElement>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const batches = useQuery({
    queryKey: ["imports"],
    queryFn: () => api.get<ImportBatchView[]>("/imports"),
    refetchInterval: 15_000,
  });

  const upload = useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      if (activeYear) form.append("academic_year_id", activeYear.id);
      return api.post<ImportBatchView>("/imports/students", form);
    },
    onSuccess: (batch) => {
      void queryClient.invalidateQueries({ queryKey: ["imports"] });
      navigate(`/imports/${batch.ID}`);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">استيراد الطلبة</h1>
        <span className="grow" />
        <input
          ref={fileRef}
          type="file"
          accept=".csv,.xlsx,.xls"
          style={{ display: "none" }}
          onChange={(e) => {
            const file = e.target.files?.[0];
            if (file) upload.mutate(file);
            e.target.value = "";
          }}
        />
        {/*
          The template before the upload, because it is the step that makes the
          upload work. An office does not have a file in this system's format;
          it has a file. Handing them the headings is cheaper than a page of
          documentation describing nine column names — and the importer accepts
          the Arabic ones anyway, in any order.
        */}
        <Button onClick={() => void api.download("/imports/template")}>تنزيل نموذج فارغ</Button>
        <Button
          variant="primary"
          disabled={!can("import.run")}
          disabledReason={reason("import.run")}
          busy={upload.isPending}
          onClick={() => fileRef.current?.click()}
        >
          رفع ملف
        </Button>
      </div>

      <div className="callout callout--note" style={{ marginTop: 10 }}>
        الاستيراد <b>لا يولّد حسابات مالية</b>. ينشئ طلبةً وتسجيلات فقط؛ التسعير أمر منفصل
        يُشغَّل بعده من <Link to="/bulk/accounts">التوليد الجماعي</Link>.
      </div>

      {refusal && (
        <div style={{ marginTop: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div style={{ marginTop: 12 }}>
        <Panel title="الدفعات" flush>
          {batches.isLoading && <Skeleton height={140} />}
          {batches.isSuccess && (batches.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا دفعات استيراد" />
          )}
          {(batches.data ?? []).length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>الملف</th>
                  <th>الحالة</th>
                  <th className="n">الصفوف</th>
                  <th className="n">صالحة</th>
                  <th className="n">أخطاء</th>
                  <th>رُفعت</th>
                </tr>
              </thead>
              <tbody>
                {(batches.data ?? []).map((batch) => (
                  <tr key={batch.ID}>
                    <td>
                      <Link to={`/imports/${batch.ID}`}>
                        {batch.SourceFilename ?? "دفعة بلا اسم ملف"}
                      </Link>
                    </td>
                    <td>
                      <BatchStatus batch={batch} />
                    </td>
                    <td className="n">{batch.TotalRows}</td>
                    <td className="n">{batch.ValidRows}</td>
                    <td className="n">{batch.ErrorRows}</td>
                    <td className="num">{formatDateTime(batch.CreatedAt)}</td>
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

/**
 * The batch status, including the stalled case.
 *
 * A batch whose heartbeat stopped mid-import is shown as stalled with a resume
 * control, not as a spinner that turns forever. §09: "no circle that spins
 * for ever".
 */
export function BatchStatus({ batch }: { batch: ImportBatchView }) {
  const stalled =
    batch.Status === "importing" &&
    batch.HeartbeatAt !== null &&
    batch.HeartbeatAt !== undefined &&
    Date.now() - new Date(batch.HeartbeatAt).getTime() > 120_000;

  if (stalled) {
    return (
      <Chip tone="void" hint={`آخر نبضة قبل ${elapsed(batch.HeartbeatAt!)}`}>
        متوقفة — تحتاج استئنافاً
      </Chip>
    );
  }

  switch (batch.Status) {
    case "uploaded":
      return <Chip tone="muted">مرفوعة</Chip>;
    case "validating":
      return <Chip tone="pending">قيد التحقق</Chip>;
    case "validated":
      return <Chip tone="pending">تحقّقت — تنتظر مراجعة</Chip>;
    case "confirmed":
      return <Chip tone="pending">مؤكَّدة — تنتظر تنفيذاً</Chip>;
    case "importing":
      return <Chip tone="pending">قيد التنفيذ</Chip>;
    case "completed":
      return <Chip tone="live">مكتملة</Chip>;
    case "failed":
      return <Chip tone="void">فشلت</Chip>;
    case "cancelled":
      return <Chip tone="muted">ملغاة</Chip>;
    default:
      return <Chip tone="muted">{batch.Status}</Chip>;
  }
}
