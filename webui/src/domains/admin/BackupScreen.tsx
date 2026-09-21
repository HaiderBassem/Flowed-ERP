import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, Refusal } from "@/api/errors";
import type { BackupScheduleView, BackupView, RestoreView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, Callout, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDateTime } from "@/lib/dates";

type Tab = "backups" | "restores";

const KIND_LABEL: Record<BackupView["kind"], string> = {
  manual: "يدوية",
  automatic: "تلقائية",
  safety: "أمان",
  imported: "مستوردة",
};

const INTERVAL_OPTIONS: [number, string][] = [
  [6, "كل 6 ساعات"],
  [12, "كل 12 ساعة"],
  [24, "كل 24 ساعة"],
  [72, "كل 3 أيام"],
  [168, "كل 7 أيام"],
];

/**
 * Backup & Restore, for an operator with no reason to know pg_dump exists.
 *
 * "Create Backup" and the schedule are one action each. Restore is the one
 * command on this screen with a cost of being wrong, so it is the one command
 * here that asks twice: a plain-language warning naming exactly what is about
 * to happen, and only then the button that does it. Nothing on this screen
 * ever shows a database name, a file path, or a shell command — those live in
 * the server's own log, not in front of someone who was told never to need one.
 */
export function BackupScreen() {
  const [tab, setTab] = useState<Tab>("backups");
  const { can, reason } = useSession();
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () => api.post<BackupView>("/backups", {}),
    onSuccess: () => {
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["backups"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">النسخ الاحتياطي</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("backup.manage")}
          disabledReason={reason("backup.manage")}
          busy={create.isPending}
          onClick={() => create.mutate()}
          title="ينشئ نسخة كاملة من قاعدة البيانات الآن ويتحقق منها قبل إضافتها للسجل"
        >
          إنشاء نسخة احتياطية
        </Button>
      </div>

      {refusal && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      {create.isPending && (
        <div style={{ marginBottom: 12 }}>
          <Callout variant="note">جارٍ إنشاء النسخة الاحتياطية والتحقق منها…</Callout>
        </div>
      )}

      <ScheduleCard />
      <ImportCard />

      <div className="cluster" style={{ marginBottom: 12 }}>
        {(
          [
            ["backups", "النسخ المحفوظة"],
            ["restores", "سجل الاسترجاع"],
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

      {tab === "backups" && <BackupsTab />}
      {tab === "restores" && <RestoresTab />}
    </main>
  );
}

/* ------------------------------------------------------------------ schedule */

function ScheduleCard() {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [draft, setDraft] = useState<{ enabled: boolean; interval: number; retention: number } | null>(
    null,
  );

  const schedule = useQuery({
    queryKey: ["backups", "schedule"],
    queryFn: () => api.get<BackupScheduleView>("/backups/schedule"),
  });

  const save = useMutation({
    mutationFn: (next: { enabled: boolean; interval: number; retention: number }) =>
      api.put<BackupScheduleView>("/backups/schedule", {
        enabled: next.enabled,
        interval_hours: next.interval,
        retention_count: next.retention,
      }),
    onSuccess: (value) => {
      setRefusal(null);
      setDraft(null);
      queryClient.setQueryData(["backups", "schedule"], value);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (schedule.isLoading) return <Skeleton height={80} />;
  if (!schedule.data) return null;

  const current = draft ?? {
    enabled: schedule.data.enabled,
    interval: schedule.data.interval_hours,
    retention: schedule.data.retention_count,
  };
  const dirty = draft !== null;

  const set = (patch: Partial<typeof current>) => setDraft({ ...current, ...patch });

  return (
    <Panel
      title="النسخ التلقائي"
      aside={
        schedule.data.last_run_at ? (
          <span className="note">آخر نسخة تلقائية {formatDateTime(schedule.data.last_run_at)}</span>
        ) : (
          <span className="note">لم تُنشأ نسخة تلقائية بعد</span>
        )
      }
    >
      {refusal && (
        <div style={{ marginBottom: 8 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster" style={{ alignItems: "center", gap: 16 }}>
        <label className="cluster" style={{ alignItems: "center", gap: 6 }}>
          <input
            type="checkbox"
            checked={current.enabled}
            disabled={!can("backup.manage")}
            onChange={(e) => set({ enabled: e.target.checked })}
          />
          تفعيل النسخ التلقائي
        </label>

        <label className="cluster" style={{ alignItems: "center", gap: 6 }}>
          <span className="label">التكرار</span>
          <select
            className="input"
            value={current.interval}
            disabled={!can("backup.manage") || !current.enabled}
            onChange={(e) => set({ interval: Number(e.target.value) })}
          >
            {INTERVAL_OPTIONS.map(([hours, label]) => (
              <option key={hours} value={hours}>
                {label}
              </option>
            ))}
          </select>
        </label>

        <label className="cluster" style={{ alignItems: "center", gap: 6 }}>
          <span className="label">عدد النسخ المحفوظة</span>
          <input
            className="input"
            type="number"
            min={1}
            style={{ width: 80 }}
            value={current.retention}
            disabled={!can("backup.manage")}
            onChange={(e) => set({ retention: Math.max(1, Number(e.target.value) || 1) })}
          />
        </label>

        {dirty && (
          <>
            <Button
              size="sm"
              variant="primary"
              disabled={!can("backup.manage")}
              disabledReason={reason("backup.manage")}
              busy={save.isPending}
              onClick={() => save.mutate(current)}
            >
              حفظ
            </Button>
            <Button size="sm" onClick={() => setDraft(null)}>
              تراجع
            </Button>
          </>
        )}
      </div>
      <p className="note" style={{ margin: "8px 0 0" }}>
        القديم يُحذف تلقائياً بعد الاحتفاظ بأحدث العدد المحدد — النسخ الأخيرة تبقى دائماً حتى لو
        كانت أقدم من ذلك.
      </p>
    </Panel>
  );
}

/* -------------------------------------------------------------------- import */

function ImportCard() {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [file, setFile] = useState<File | null>(null);

  const upload = useMutation({
    mutationFn: () => {
      const form = new FormData();
      form.append("file", file!);
      return api.post<BackupView>("/backups/import", form);
    },
    onSuccess: (run) => {
      setFile(null);
      if (run.status === "failed") {
        setRefusal(
          new Refusal({
            status: 200,
            code: "backup.import.invalid",
            kind: "validation",
            message: run.error ?? "هذا الملف ليس نسخة احتياطية صالحة",
          }),
        );
      } else {
        setRefusal(null);
      }
      void queryClient.invalidateQueries({ queryKey: ["backups"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="استيراد نسخة احتياطية">
      <p className="note" style={{ marginTop: 0 }}>
        اختر ملف نسخة احتياطية من هذا النظام — سواء صُدِّر من هذا الجهاز أو من جهاز آخر يشغّل نفس
        التطبيق — ليتم التحقق منه وإضافته إلى القائمة أدناه.
      </p>
      {refusal && (
        <div style={{ marginBottom: 8 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}
      <div className="cluster" style={{ alignItems: "center" }}>
        <input
          type="file"
          disabled={!can("backup.manage")}
          onChange={(e) => setFile(e.target.files?.[0] ?? null)}
        />
        <Button
          variant="primary"
          disabled={!can("backup.manage") || !file}
          disabledReason={!can("backup.manage") ? reason("backup.manage") : "اختر ملفاً أولاً"}
          busy={upload.isPending}
          onClick={() => upload.mutate()}
        >
          استيراد
        </Button>
      </div>
    </Panel>
  );
}

/* ------------------------------------------------------------------ backups */

function BackupsTab() {
  const backups = useQuery({
    queryKey: ["backups"],
    queryFn: () => api.get<BackupView[]>("/backups", { query: { limit: 100 } }),
  });

  const rows = backups.data ?? [];

  if (backups.isLoading) return <Skeleton height={160} />;

  if (backups.isError) {
    return isRefusal(backups.error) ? (
      <RefusalPanel refusal={backups.error} onRetry={() => void backups.refetch()} />
    ) : (
      <EmptyState kind="no-results" title="تعذّر تحميل النسخ" />
    );
  }

  if (rows.length === 0) {
    return (
      <Panel>
        <EmptyState
          kind="not-yet"
          title="لا توجد نسخ احتياطية بعد"
          detail="اضغط «إنشاء نسخة احتياطية» أعلاه لإنشاء أول نسخة."
        />
      </Panel>
    );
  }

  return (
    <div className="stack">
      {rows.map((run) => (
        <BackupRow key={run.id} run={run} />
      ))}
    </div>
  );
}

function BackupRow({ run }: { run: BackupView }) {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [confirming, setConfirming] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["backups"] });
    void queryClient.invalidateQueries({ queryKey: ["backups", "restores"] });
  };

  const restore = useMutation({
    mutationFn: () => api.post<RestoreView>(`/backups/${run.id}/restore`, {}),
    onSuccess: () => {
      setConfirming(false);
      setRefusal(null);
      invalidate();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const remove = useMutation({
    mutationFn: () => api.delete(`/backups/${run.id}`),
    onSuccess: invalidate,
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const canManage = can("backup.manage");
  const canRestore = canManage && run.verified;
  const canDelete = canManage && run.status !== "running";

  return (
    <Panel
      title={
        <>
          <span>{formatDateTime(run.started_at)}</span> <KindChip kind={run.kind} />{" "}
          <StatusChip run={run} />
        </>
      }
      aside={<span className="label">{formatBytes(run.bytes)}</span>}
    >
      {run.error && (
        <p className="note" style={{ color: "var(--void)" }}>
          {run.error}
        </p>
      )}

      {refusal && (
        <div style={{ marginBottom: 8 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      {confirming && (
        <div style={{ margin: "8px 0" }}>
          <Callout variant="warn">
            سيتم استبدال البيانات الحالية بهذه النسخة. سيُحفظ نسخة أمان من البيانات الحالية أولاً
            تلقائياً، وإذا فشل أي فحص فسيُلغى الاسترجاع ولن تُلمَس البيانات الحالية.
          </Callout>
        </div>
      )}

      {restore.isPending && (
        <div style={{ margin: "8px 0" }}>
          <Callout variant="note">جارٍ الاسترجاع — لا تُغلق هذه الصفحة…</Callout>
        </div>
      )}

      <div className="cluster">
        {!confirming ? (
          <Button
            size="sm"
            disabled={!canRestore}
            disabledReason={!canManage ? reason("backup.manage") : "النسخة غير موثّقة"}
            onClick={() => setConfirming(true)}
          >
            استرجاع
          </Button>
        ) : (
          <>
            <Button
              size="sm"
              variant="danger"
              busy={restore.isPending}
              onClick={() => restore.mutate()}
            >
              أؤكد الاسترجاع
            </Button>
            <Button size="sm" disabled={restore.isPending} onClick={() => setConfirming(false)}>
              إلغاء
            </Button>
          </>
        )}

        <Button
          size="sm"
          disabled={!run.verified}
          disabledReason="النسخة غير موثّقة"
          onClick={() => void api.download(`/backups/${run.id}/export`)}
        >
          تصدير
        </Button>

        <Button
          size="sm"
          variant="danger"
          disabled={!canDelete}
          disabledReason={reason("backup.manage")}
          busy={remove.isPending}
          onClick={() => remove.mutate()}
        >
          حذف
        </Button>
      </div>
    </Panel>
  );
}

/* ----------------------------------------------------------------- restores */

function RestoresTab() {
  const restores = useQuery({
    queryKey: ["backups", "restores"],
    queryFn: () => api.get<RestoreView[]>("/backups/restores", { query: { limit: 100 } }),
  });

  const rows = restores.data ?? [];

  if (restores.isLoading) return <Skeleton height={140} />;

  if (rows.length === 0) {
    return (
      <Panel>
        <EmptyState kind="not-yet" title="لا عمليات استرجاع مسجّلة" />
      </Panel>
    );
  }

  return (
    <div className="stack">
      {rows.map((run) => (
        <Panel
          key={run.id}
          title={
            <>
              <span>{formatDateTime(run.started_at)}</span> <RestoreStatusChip status={run.status} />
            </>
          }
        >
          {run.error && (
            <p className="note" style={{ color: "var(--void)" }}>
              {run.error}
            </p>
          )}
          {run.checks && run.checks.length > 0 && (
            <ul style={{ margin: "8px 0 0", paddingInlineStart: 18 }}>
              {run.checks.map((check) => (
                <li key={check.name} className="note">
                  {check.passed ? "✓" : "✗"} {check.name}
                  {check.detail ? ` — ${check.detail}` : ""}
                </li>
              ))}
            </ul>
          )}
        </Panel>
      ))}
    </div>
  );
}

/* --------------------------------------------------------------------- misc */

function KindChip({ kind }: { kind: BackupView["kind"] }) {
  const tone = kind === "safety" ? "pending" : kind === "imported" ? "muted" : "live";
  return <Chip tone={tone}>{KIND_LABEL[kind]}</Chip>;
}

function StatusChip({ run }: { run: BackupView }) {
  if (run.status === "running") return <Chip tone="pending">جارية</Chip>;
  if (run.status === "failed") return <Chip tone="void" hint={run.error ?? undefined}>فشلت</Chip>;
  return <Chip tone="live">موثّقة</Chip>;
}

function RestoreStatusChip({ status }: { status: RestoreView["status"] }) {
  switch (status) {
    case "restored":
      return <Chip tone="live">تم الاسترجاع</Chip>;
    case "failed":
      return <Chip tone="void">فشل</Chip>;
    default:
      return <Chip tone="pending">{status}</Chip>;
  }
}

function formatBytes(bytes: number): string {
  if (!bytes) return "—";
  const units = ["بايت", "كيلوبايت", "ميغابايت", "غيغابايت"];
  let value = bytes;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit += 1;
  }
  return `${value.toFixed(unit === 0 ? 0 : 1)} ${units[unit]}`;
}
