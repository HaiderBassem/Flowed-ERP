import { useMemo, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { InstallmentTemplateView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";

/**
 * Installment templates — §09.
 *
 * The rule: the shares must sum to exactly 100%, or exactly one line carries
 * the remainder. §09 is explicit about why it is validated **live while the
 * template is built** rather than at save: discovering the split is broken on
 * registration day, across hundreds of students, is far too late.
 *
 * Shares are basis points on the wire (10000 = 100%), matching the server's
 * money.BasisPoints. Percentages are only ever a rendering of them, so a
 * template recomputed in five years is bit-identical.
 */
export function InstallmentTemplatesScreen() {
  const { activeYear } = useWorkingContext();
  const { can, reason } = useSession();
  const [building, setBuilding] = useState(false);

  const templates = useQuery({
    queryKey: ["installment-templates", activeYear?.id],
    queryFn: () =>
      api.get<InstallmentTemplateView[]>("/installment-templates", {
        query: { academic_year_id: activeYear?.id },
      }),
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">قوالب الأقساط</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("config.write")}
          disabledReason={reason("config.write")}
          onClick={() => setBuilding((b) => !b)}
        >
          {building ? "إغلاق البنّاء" : "قالب جديد"}
        </Button>
      </div>

      {building && <TemplateBuilder onDone={() => setBuilding(false)} />}

      <div style={{ marginTop: 12 }}>
        <Panel title="القوالب" flush>
          {templates.isLoading && <Skeleton height={140} />}
          {templates.isError &&
            (isRefusal(templates.error) ? (
              <RefusalPanel refusal={templates.error} onRetry={() => void templates.refetch()} />
            ) : (
              <EmptyState kind="no-results" title="تعذّر جلب القوالب" />
            ))}
          {templates.isSuccess && (templates.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا قوالب معرّفة" />
          )}
          {(templates.data ?? []).map((template) => (
            <TemplateCard key={template.id} template={template} />
          ))}
        </Panel>
      </div>
    </main>
  );
}

function TemplateCard({ template }: { template: InstallmentTemplateView }) {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const publish = useMutation({
    mutationFn: () => api.post(`/installment-templates/${template.id}/publish`, {}),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["installment-templates"] }),
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const balanced = template.total_bp === 10000;

  return (
    <div style={{ borderBottom: "1px solid var(--rule)", padding: "12px 14px" }}>
      <div className="cluster">
        <b>{template.name_ar}</b>
        <span className="k">{template.code}</span>
        {template.status === "published" ? (
          <Chip tone="live">منشور</Chip>
        ) : (
          <Chip tone="pending">مسودة</Chip>
        )}
        {!balanced && (
          <Chip tone="void" hint="مجموع الحصص لا يساوي 100٪">
            غير متوازن
          </Chip>
        )}
        <span className="grow" />
        <span className="label">
          الحد الأقصى <span className="num">{template.max_installments}</span> قسطاً · التخصيص{" "}
          <span className="num">{template.specificity_score}</span>
        </span>
        {template.status !== "published" && (
          <Button
            size="sm"
            disabled={!can("config.write") || !balanced}
            disabledReason={
              !can("config.write")
                ? reason("config.write")
                : !balanced
                  ? "لا يُنشر قالب مجموع حصصه ليس 100٪"
                  : undefined
            }
            busy={publish.isPending}
            onClick={() => publish.mutate()}
          >
            نشر
          </Button>
        )}
      </div>

      <table className="grid" style={{ marginTop: 8 }}>
        <thead>
          <tr>
            <th className="n">#</th>
            <th>الوصف</th>
            <th className="n">الحصة</th>
            <th className="n">الاستحقاق بعد</th>
          </tr>
        </thead>
        <tbody>
          {template.lines.map((line) => (
            <tr key={line.line_no}>
              <td className="n">{line.line_no}</td>
              <td>{line.label_ar ?? "—"}</td>
              <td className="n">{line.share_percent}</td>
              <td className="n">{line.due_offset_days} يوم</td>
            </tr>
          ))}
        </tbody>
      </table>

      <p className="note" style={{ marginTop: 6 }}>
        مجموع الحصص <span className="num">{(template.total_bp / 100).toFixed(2)}</span>٪
        {balanced ? " — متوازن" : " — يجب أن يبلغ 100٪"}
      </p>

      {refusal && (
        <div style={{ marginTop: 8 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}
    </div>
  );
}

interface DraftLine {
  sharePercent: string;
  dueOffsetDays: string;
  label: string;
}

/**
 * The builder, validated as it is typed.
 *
 * The running total is shown on every keystroke and the save control stays
 * disabled until it reaches exactly 100%.
 */
function TemplateBuilder({ onDone }: { onDone: () => void }) {
  const queryClient = useQueryClient();
  const { activeYear } = useWorkingContext();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [lines, setLines] = useState<DraftLine[]>([
    { sharePercent: "50", dueOffsetDays: "0", label: "القسط الأول" },
    { sharePercent: "50", dueOffsetDays: "120", label: "القسط الثاني" },
  ]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  // Basis points, computed from what was typed. Percentages are the rendering;
  // the wire and the server both speak basis points.
  const totalBp = useMemo(
    () =>
      lines.reduce((sum, line) => {
        const pct = Number(line.sharePercent);
        return sum + (Number.isFinite(pct) ? Math.round(pct * 100) : 0);
      }, 0),
    [lines],
  );

  const balanced = totalBp === 10000;

  const create = useMutation({
    mutationFn: () =>
      api.post("/installment-templates", {
        code: code.trim(),
        name_ar: name.trim(),
        academic_year_id: activeYear?.id,
        lines: lines.map((line, index) => ({
          line_no: index + 1,
          share_bp: Math.round(Number(line.sharePercent) * 100),
          due_offset_days: Number(line.dueOffsetDays) || 0,
          label_ar: line.label.trim() || undefined,
        })),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["installment-templates"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const update = (index: number, patch: Partial<DraftLine>) =>
    setLines((current) => current.map((line, i) => (i === index ? { ...line, ...patch } : line)));

  return (
    <Panel title="بناء قالب" aside={<span className="label">التحقق حيّ أثناء البناء</span>}>
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

      <span className="field__label">السطور</span>
      <table className="grid" style={{ marginBottom: 10 }}>
        <thead>
          <tr>
            <th className="n">#</th>
            <th>الوصف</th>
            <th className="n">الحصة ٪</th>
            <th className="n">الاستحقاق بعد (يوم)</th>
            <th />
          </tr>
        </thead>
        <tbody>
          {lines.map((line, index) => (
            <tr key={index}>
              <td className="n">{index + 1}</td>
              <td>
                <input
                  className="input"
                  value={line.label}
                  onChange={(e) => update(index, { label: e.target.value })}
                />
              </td>
              <td className="n">
                <input
                  className="input"
                  inputMode="decimal"
                  dir="ltr"
                  style={{ textAlign: "end" }}
                  value={line.sharePercent}
                  onChange={(e) => update(index, { sharePercent: e.target.value })}
                />
              </td>
              <td className="n">
                <input
                  className="input"
                  inputMode="numeric"
                  dir="ltr"
                  style={{ textAlign: "end" }}
                  value={line.dueOffsetDays}
                  onChange={(e) => update(index, { dueOffsetDays: e.target.value })}
                />
              </td>
              <td>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={lines.length <= 1}
                  disabledReason={lines.length <= 1 ? "لا بد من سطر واحد على الأقل" : undefined}
                  onClick={() => setLines((c) => c.filter((_, i) => i !== index))}
                >
                  حذف
                </Button>
              </td>
            </tr>
          ))}
        </tbody>
      </table>

      <div className="cluster" style={{ marginBottom: 10 }}>
        <Button
          size="sm"
          onClick={() =>
            setLines((c) => [...c, { sharePercent: "0", dueOffsetDays: "0", label: "" }])
          }
        >
          + سطر
        </Button>
        <span className="grow" />
        <span className={balanced ? "" : "money money--neg"}>
          المجموع <span className="num">{(totalBp / 100).toFixed(2)}</span>٪
        </span>
        {balanced ? (
          <Chip tone="live">متوازن</Chip>
        ) : (
          <Chip tone="void">
            {totalBp > 10000 ? "يتجاوز 100٪" : `ناقص ${((10000 - totalBp) / 100).toFixed(2)}٪`}
          </Chip>
        )}
      </div>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!balanced || !code.trim() || !name.trim()}
          disabledReason={
            !balanced
              ? "مجموع الحصص يجب أن يبلغ 100٪ بالضبط"
              : !code.trim() || !name.trim()
                ? "أكمل الرمز والاسم"
                : undefined
          }
          busy={create.isPending}
          onClick={() => create.mutate()}
        >
          حفظ كمسودة
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
        <span className="grow" />
        <span className="note">يُحفظ مسودةً؛ النشر خطوة منفصلة.</span>
      </div>
    </Panel>
  );
}
