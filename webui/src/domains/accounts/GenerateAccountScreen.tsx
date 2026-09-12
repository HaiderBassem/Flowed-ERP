import { useState } from "react";
import { useNavigate, useSearchParams } from "react-router-dom";
import { useMutation, useQuery } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { EnrollmentView, GenerateAccountResult, InstallmentTemplateView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Waterfall } from "@/components/Waterfall";
import { Button, EmptyState, Panel, Row } from "@/components/primitives";
import { InstallmentTable } from "./InstallmentTable";
import { useSession } from "@/app/session";

/**
 * Generating one account — §09.
 *
 * The preview is mandatory and this screen enforces it: the commit control
 * does not exist until a dry run has been read. What gets frozen here cannot
 * be edited afterwards — only adjusted by a signed entry — so the winning
 * policy, its specificity, the fee lines, the discounts and the schedule are
 * all shown first.
 *
 * The dry run computes everything and writes nothing, so a finance manager
 * approves real figures rather than a promise.
 */
export function GenerateAccountScreen() {
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const { can, reason } = useSession();

  const enrollmentId = params.get("enrollment") ?? "";
  const [templateId, setTemplateId] = useState("");
  const [preview, setPreview] = useState<GenerateAccountResult | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [key] = useState(() => newIdempotencyKey());

  const enrollment = useQuery({
    queryKey: ["enrollment", enrollmentId],
    queryFn: () => api.get<EnrollmentView>(`/enrollments/${enrollmentId}`),
    enabled: Boolean(enrollmentId),
  });

  const templates = useQuery({
    queryKey: ["installment-templates"],
    queryFn: () => api.get<InstallmentTemplateView[]>("/installment-templates"),
  });

  const run = useMutation({
    mutationFn: (dryRun: boolean) =>
      api.post<GenerateAccountResult>(
        "/accounts",
        {
          enrollment_id: enrollmentId,
          ...(templateId ? { installment_template_id: templateId } : {}),
          dry_run: dryRun,
        },
        // Keyed even for generation: a retried generation that created a
        // second account would give one enrollment two sets of prices.
        dryRun ? {} : { idempotencyKey: key },
      ),
    onSuccess: (result) => {
      setRefusal(null);
      if (result.dry_run) {
        setPreview(result);
      } else {
        navigate(`/accounts/${result.account.id}`);
      }
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  if (!enrollmentId) {
    return (
      <main className="screen">
        <EmptyState
          kind="not-yet"
          title="لا تسجيل محدَّد"
          detail="يُفتح هذا الأمر من صفحة التسجيل — الحساب يخص تسجيلاً بعينه."
        />
      </main>
    );
  }

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: "التسجيل", to: `/enrollments/${enrollmentId}` },
          { label: "توليد حساب" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">توليد حساب</h1>
        <span className="grow" />
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <Panel title="المدخلات">

          {enrollment.data && (
            <>
              <Row label="المرحلة">
                <span className="num">{enrollment.data.stage}</span>
              </Row>
              <Row label="المحاولة">
                <span className="num">{enrollment.data.attempt_number}</span>
                {enrollment.data.is_repeat && <Chip tone="pending">معيد</Chip>}
              </Row>
            </>
          )}

          <label className="field" style={{ marginTop: 10 }}>
            <span className="field__label">قالب الأقساط</span>
            <select
              className="input"
              value={templateId}
              onChange={(e) => setTemplateId(e.target.value)}
            >
              <option value="">— يُختار بالترجيح —</option>
              {(templates.data ?? [])
                .filter((t) => t.status === "published")
                .map((t) => (
                  <option key={t.id} value={t.id}>
                    {t.name_ar} ({t.code})
                  </option>
                ))}
            </select>
            <span className="field__hint">
              اتركه فارغاً ليختار النظام القالب الأكثر تخصيصاً، كما يفعل التوليد الجماعي.
            </span>
          </label>

          {refusal && (
            <div style={{ marginBottom: 10 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <div className="cluster">
            <Button
              variant="primary"
              disabled={!can("account.generate")}
              disabledReason={reason("account.generate")}
              busy={run.isPending && run.variables === true}
              onClick={() => run.mutate(true)}
            >
              معاينة (لا يكتب شيئاً)
            </Button>
          </div>
        </Panel>

        {preview ? (
          <div className="stack">
            <Panel
              title="المعاينة"
              aside={<Chip tone="pending">لم يُكتب شيء</Chip>}
            >
              <Waterfall
                account={preview.account}
                components={preview.fee_components}
                discounts={preview.discounts}
              />
            </Panel>

            <Panel title="الخطة المقترحة" flush>
              <InstallmentTable installments={preview.installments} compact />
            </Panel>

            <Panel title="الالتزام">
              <p className="note" style={{ marginBottom: 10 }}>
                ما يُجمَّد هنا لا يُعدَّل بعدها إلا بقيد تسوية موقّع. اقرأ السياسة الفائزة وسطور
                الرسم والخصومات قبل الاعتماد.
              </p>
              <Row label="الصافي الذي سيُجمَّد">
                <Money value={preview.account.net_snapshot} size="big" tone="plain" />
              </Row>
              <div className="cluster" style={{ marginTop: 10 }}>
                <Button
                  variant="primary"
                  disabled={!can("account.generate")}
                  disabledReason={reason("account.generate")}
                  busy={run.isPending && run.variables === false}
                  onClick={() => run.mutate(false)}
                >
                  اعتماد وتوليد
                </Button>
                <Button variant="ghost" onClick={() => setPreview(null)}>
                  إعادة المعاينة
                </Button>
              </div>
            </Panel>
          </div>
        ) : (
          <Panel title="المعاينة">
            <EmptyState
              kind="not-yet"
              title="لم تُشغَّل المعاينة بعد"
              detail="لا يوجد زر توليد قبل قراءة معاينة — التسعير المجمَّد لا يُتراجع عنه إلا بقيود."
            />
          </Panel>
        )}
      </div>
    </main>
  );
}
