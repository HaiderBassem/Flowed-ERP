import { useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { ImportFinding, ImportReviewView, ImportRowView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton, Stat } from "@/components/primitives";
import { useSession } from "@/app/session";
import { labelDisposition } from "@/design/lexicon";
import { BatchStatus } from "./ImportsScreen";

const STAGES = ["رفع", "تحقق", "مراجعة", "تأكيد", "تنفيذ"] as const;

/**
 * The import wizard — §09.
 *
 * Five stages that mirror the batch's own states rather than inventing a
 * parallel progress model, so a batch that a worker abandoned shows the state
 * it is actually in.
 *
 * Three rules shape the review step:
 *   - errors are **grouped by kind** ("34 rows with no mother's name"), not
 *     listed one by one, and each group offers a bulk decision;
 *   - editing any row sends the batch back to validation, because a row that
 *     changed was not validated in the state it is now in;
 *   - overriding a rejected row requires a written reason, which is the
 *     audited confirmation that this really is the same student.
 *
 * Confirmation is gated on unresolved errors reaching zero.
 */
export function ImportScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const review = useQuery({
    queryKey: ["import", id],
    queryFn: () => api.get<ImportReviewView>(`/imports/${id}`, { query: { limit: 500 } }),
    enabled: Boolean(id),
    refetchInterval: (query) => {
      const status = query.state.data?.batch.Status;
      return status === "validating" || status === "importing" ? 3000 : false;
    },
  });

  const act = useMutation({
    mutationFn: (action: "validate" | "confirm" | "run" | "resume" | "cancel") =>
      api.post(`/imports/${id}/${action}`, action === "cancel" ? { reason: "ألغاها المراجع" } : {}),
    onSuccess: () => {
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["import", id] });
      void queryClient.invalidateQueries({ queryKey: ["imports"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const disposition = useMutation({
    mutationFn: (input: { rowNo: number; disposition: string; reason?: string }) =>
      api.patch(`/imports/${id}/rows/${input.rowNo}`, {
        disposition: input.disposition,
        ...(input.reason ? { reason: input.reason } : {}),
      }),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["import", id] }),
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (review.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={240} />
      </main>
    );
  }

  if (review.isError) {
    return (
      <main className="screen">
        {isRefusal(review.error) ? (
          <RefusalPanel refusal={review.error} onRetry={() => void review.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح الدفعة" />
        )}
      </main>
    );
  }

  const data = review.data!;
  const batch = data.batch;
  const counts = data.counts;

  const stageIndex = stageOf(batch.Status);
  const stalled =
    batch.Status === "importing" &&
    batch.HeartbeatAt &&
    Date.now() - new Date(batch.HeartbeatAt).getTime() > 120_000;

  const writable = can("import.run");

  return (
    <main className="screen screen--wide">
      <Crumbs
        items={[
          { label: "الاستيراد", to: "/imports" },
          { label: batch.SourceFilename ?? "دفعة استيراد" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">{batch.SourceFilename ?? "دفعة استيراد"}</h1>
        <BatchStatus batch={batch} />
      </div>

      <div className="cluster" style={{ marginBottom: 12 }}>
        {STAGES.map((stage, index) => (
          <span
            key={stage}
            className="sheet__stage"
            data-state={
              index === stageIndex ? "current" : index < stageIndex ? "done" : undefined
            }
          >
            {index + 1} · {stage}
          </span>
        ))}
      </div>

      <div className="deck" style={{ marginBottom: 12 }}>
        <Stat value={counts.Total} label="الصفوف" />
        <Stat value={counts.Valid} label="صالحة" tone="live" />
        <Stat value={counts.Warning} label="تحذيرات" tone="pending" />
        <Stat value={counts.UnresolvedErrors} label="أخطاء غير محسومة" tone="void" />
        <Stat value={counts.Created} label="أُنشئ" tone="live" />
        <Stat value={counts.Skipped} label="تُخطّي" tone="muted" />
      </div>

      {stalled && (
        <div className="refusal" style={{ marginBottom: 12 }}>
          <p className="refusal__title">توقّف نبض هذه الدفعة</p>
          <p className="refusal__body">
            العامل الذي كان ينفّذها لم يعد يستجيب. الصفوف التي التزمت محفوظة — الاستئناف يكمل من
            حيث توقّف ولا يعيد ما نُفِّذ.
          </p>
          <div className="refusal__actions">
            <Button
              variant="primary"
              disabled={!writable}
              disabledReason={reason("import.run")}
              busy={act.isPending}
              onClick={() => act.mutate("resume")}
            >
              استئناف
            </Button>
          </div>
        </div>
      )}

      {batch.ErrorSummary && (
        <div className="refusal" style={{ marginBottom: 12 }}>
          <p className="refusal__title">ملخص العطل</p>
          <p className="refusal__body" style={{ margin: 0 }}>
            {batch.ErrorSummary}
          </p>
        </div>
      )}

      {refusal && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <ErrorGroups
        rows={data.rows}
        writable={writable}
        onBulkSkip={(code) => {
          // Skipping a whole group is one decision, so it is one action — not
          // thirty-four identical clicks.
          for (const row of data.rows) {
            if ((row.Errors ?? []).some((e) => e.code === code)) {
              disposition.mutate({
                rowNo: row.RowNo,
                disposition: "skip",
                reason: `تخطٍّ جماعي للمجموعة ${code}`,
              });
            }
          }
        }}
      />

      <Panel title="الصفوف" flush>
        <RowGrid rows={data.rows} />
      </Panel>

      <div className="cluster" style={{ marginTop: 12 }}>
        <Button
          disabled={!writable || batch.Status === "importing"}
          disabledReason={!writable ? reason("import.run") : undefined}
          busy={act.isPending}
          onClick={() => act.mutate("validate")}
        >
          إعادة التحقق
        </Button>
        <Button
          variant="primary"
          disabled={!writable || counts.UnresolvedErrors > 0 || batch.Status !== "validated"}
          disabledReason={
            counts.UnresolvedErrors > 0
              ? `احسم ${counts.UnresolvedErrors} صفاً بخطأ أولاً — التأكيد مشروط ببلوغها صفراً`
              : batch.Status !== "validated"
                ? "الدفعة ليست في حالة «تحقّقت»"
                : undefined
          }
          busy={act.isPending}
          onClick={() => act.mutate("confirm")}
        >
          تأكيد
        </Button>
        <Button
          variant="primary"
          disabled={!writable || batch.Status !== "confirmed"}
          disabledReason={batch.Status !== "confirmed" ? "أكِّد الدفعة أولاً" : undefined}
          busy={act.isPending}
          onClick={() => act.mutate("run")}
        >
          تنفيذ
        </Button>
        <span className="grow" />
        <Button
          variant="danger"
          disabled={!writable || batch.Status === "completed"}
          disabledReason={batch.Status === "completed" ? "اكتملت — لا تُلغى" : undefined}
          onClick={() => act.mutate("cancel")}
        >
          إلغاء الدفعة
        </Button>
      </div>

      {batch.Status === "completed" && (
        <div className="callout" style={{ marginTop: 12 }}>
          <b>اكتمل الاستيراد.</b> أُنشئ <span className="num">{counts.Created}</span> وحُدِّث{" "}
          <span className="num">{counts.Updated}</span>. لم تُولَّد أي حسابات مالية — التسعير أمر
          منفصل.
        </div>
      )}

      <p className="note" style={{ marginTop: 12 }}>
        التنفيذ يلتزم صفاً صفاً: صفٌّ مسموم لا يُسقط الصفوف السليمة معه.
      </p>
    </main>
  );
}

function stageOf(status: string): number {
  switch (status) {
    case "uploaded":
      return 0;
    case "validating":
      return 1;
    case "validated":
      return 2;
    case "confirmed":
      return 3;
    case "importing":
    case "completed":
    case "failed":
      return 4;
    default:
      return 0;
  }
}

/**
 * Errors grouped by code, with a bulk decision each.
 *
 * §09: "34 rows with no mother's name" is one fact and one decision. Listing
 * thirty-four sentences is how a reviewer stops reading them.
 */
function ErrorGroups({
  rows,
  writable,
  onBulkSkip,
}: {
  rows: ImportRowView[];
  writable: boolean;
  onBulkSkip: (code: string) => void;
}) {
  const groups = useMemo(() => {
    const map = new Map<string, { finding: ImportFinding; count: number }>();
    for (const row of rows) {
      for (const finding of row.Errors ?? []) {
        const existing = map.get(finding.code);
        if (existing) existing.count += 1;
        else map.set(finding.code, { finding, count: 1 });
      }
    }
    return [...map.values()].sort((a, b) => b.count - a.count);
  }, [rows]);

  if (groups.length === 0) return null;

  return (
    <Panel title="الأخطاء، مجمّعة بالنوع" aside={<Chip tone="void">{groups.length} مجموعة</Chip>}>
      {groups.map(({ finding, count }) => (
        <div className="row" key={finding.code}>
          <span className="grow">
            <b className="num">{count}</b> صفاً — {finding.message}
            {finding.field && <span className="label"> ({finding.field})</span>}
          </span>
          <span className="k">{finding.code}</span>
          <Button
            size="sm"
            disabled={!writable}
            onClick={() => onBulkSkip(finding.code)}
            title="تخطّي كل صفوف هذه المجموعة بقرار واحد"
          >
            تخطّي المجموعة
          </Button>
        </div>
      ))}
      <p className="note" style={{ marginTop: 8 }}>
        تعديل أي صف يعيد الدفعة إلى التحقق: صفٌّ تغيّر لم يُتحقَّق منه في حالته الحالية.
      </p>
    </Panel>
  );
}

/** The dense row grid. */
function RowGrid({ rows }: { rows: ImportRowView[] }) {
  if (rows.length === 0) {
    return <EmptyState kind="not-yet" title="لا صفوف" />;
  }

  const columns = Object.keys(rows[0]!.RawData ?? {}).slice(0, 6);

  return (
    <div className="table-wrap" style={{ border: 0, maxHeight: "55vh", overflowY: "auto" }}>
      <table className="grid">
        <thead>
          <tr>
            <th className="n">#</th>
            {columns.map((column) => (
              <th key={column}>{column}</th>
            ))}
            <th>التحقق</th>
            <th>القرار</th>
          </tr>
        </thead>
        <tbody>
          {rows.map((row) => (
            <tr key={row.ID}>
              <td className="n">{row.RowNo}</td>
              {columns.map((column) => (
                <td key={column}>{String(row.RawData?.[column] ?? "—")}</td>
              ))}
              <td>
                {(row.Errors ?? []).length > 0 ? (
                  <Chip tone="void" hint={(row.Errors ?? []).map((e) => e.message).join(" · ")}>
                    خطأ
                  </Chip>
                ) : (row.Warnings ?? []).length > 0 ? (
                  <Chip
                    tone="pending"
                    hint={(row.Warnings ?? []).map((w) => w.message).join(" · ")}
                  >
                    تحذير
                  </Chip>
                ) : (
                  <Chip tone="live">صالح</Chip>
                )}
              </td>
              <td className="label">{labelDisposition(row.Disposition)}</td>
            </tr>
          ))}
        </tbody>
      </table>
    </div>
  );
}
