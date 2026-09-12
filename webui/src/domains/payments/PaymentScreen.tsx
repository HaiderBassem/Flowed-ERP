import { useState } from "react";
import { Link, useParams, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { PaymentMethodView, PaymentView } from "@/api/types";
import { StateChip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { Money, MoneyField } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDateTime } from "@/lib/dates";
import { amount, parseInput } from "@/lib/money";
import { ReceiptPreview } from "./ReceiptPreview";

/**
 * The receipt page — §09.
 *
 * A voided receipt prints too, struck through and stamped. The auditor asking
 * about it needs to see it; disappearing is not an answer.
 *
 * Two commands only, and neither is an edit: request a void, or request a
 * refund. Both open a command sheet, and both are two-signature flows.
 */
export function PaymentScreen() {
  const { id } = useParams<{ id: string }>();
  const [params] = useSearchParams();
  const { can, reason } = useSession();
  const [sheet, setSheet] = useState<"void" | "refund" | null>(null);

  const payment = useQuery({
    queryKey: ["payment", id],
    queryFn: () => api.get<PaymentView>(`/payments/${id}`),
    enabled: Boolean(id),
  });

  if (payment.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={240} />
      </main>
    );
  }

  if (payment.isError) {
    return (
      <main className="screen">
        {isRefusal(payment.error) ? (
          <RefusalPanel refusal={payment.error} onRetry={() => void payment.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح الوصل" />
        )}
      </main>
    );
  }

  const view = payment.data!;
  const voided = view.status === "voided";

  const voidBlock = !can("payment.void.request")
    ? reason("payment.void.request")
    : voided
      ? "هذا الوصل ملغى أصلاً"
      : undefined;

  const refundBlock = !can("refund.request")
    ? reason("refund.request")
    : voided
      ? "الوصل الملغى لا يُسترجَع — الإلغاء أعاد المبلغ كاملاً"
      : undefined;

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: "ملف الطالب", to: `/students/${view.student_id}` },
          { label: "الحساب المالي", to: `/accounts/${view.account_id}` },
          { label: `وصل ${view.receipt_no ?? ""}` },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">
          وصل <span className="ltr num">{view.receipt_no ?? "—"}</span>
        </h1>
        <StateChip entity="payment" status={view.status} />
        <span className="grow" />
        <Link className="btn" to={`/accounts/${view.account_id}`}>
          الحساب
        </Link>
        <Link className="btn" to={`/students/${view.student_id}`}>
          ملف الطالب
        </Link>
      </div>

      <div className="cols cols--half" style={{ marginTop: 14 }}>
        <div className="stack">
          <Panel title="الوثيقة">
            <Row label="المبلغ">
              <Money value={view.amount} size="big" tone={voided ? "plain" : "positive"} />
            </Row>
            <Row label="قُبض">
              <span className="num">{formatDateTime(view.paid_at)}</span>
            </Row>
            <Row label="رُحِّل">
              <span className="num">{formatDateTime(view.posted_at ?? null)}</span>
            </Row>
            {view.payer_name && <Row label="الدافع">{view.payer_name}</Row>}
            {view.method_reference && (
              <Row label="المرجع">
                <span className="ltr num">{view.method_reference}</span>
              </Row>
            )}
            {voided && (
              <>
                <Row label="أُلغي">
                  <span className="num">{formatDateTime(view.voided_at ?? null)}</span>
                </Row>
                <Row label="سبب الإلغاء">{view.void_reason ?? "—"}</Row>
              </>
            )}
          </Panel>

          <Panel title="التصحيح">
            {/* No edit, no delete — their absence is the design. */}
            <p className="note" style={{ marginBottom: 10 }}>
              لا يُعدَّل وصل ولا يُحذف. الأمران أدناه ينشئان وثيقة جديدة، ولكل منهما توقيعان.
            </p>
            <div className="cluster">
              <Button
                variant="danger"
                disabled={Boolean(voidBlock)}
                disabledReason={voidBlock}
                onClick={() => setSheet("void")}
                title="إلغاء كامل للوصل — يشترط ألا يكون عليه استرجاع مُرحَّل"
              >
                طلب إلغاء
              </Button>
              <Button
                disabled={Boolean(refundBlock)}
                disabledReason={refundBlock}
                onClick={() => setSheet("refund")}
                title="إعادة جزء أو كل المبلغ مع بقاء الوصل قائماً"
              >
                طلب استرجاع
              </Button>
            </div>
            <p className="note" style={{ marginTop: 10 }}>
              <b>الفرق:</b> الإلغاء يُبطل الوصل كله ويُشترط ألا يكون عليه استرجاع مُرحَّل — لأن
              إلغاء وصل بعد استرجاع جزئي يعني صرف أكثر مما قُبض. الاسترجاع يُعيد مبلغاً ويُبقي
              الوصل قائماً، وهو الأمر الصحيح بعد إغلاق الوردية.
            </p>
          </Panel>

          {sheet === "void" && (
            <VoidSheet payment={view} onClose={() => setSheet(null)} />
          )}
          {sheet === "refund" && (
            <RefundSheet payment={view} onClose={() => setSheet(null)} />
          )}
        </div>

        <Panel title="الوصل كما يُطبع">
          <ReceiptPreview kind="payments" id={view.id} autoPrint={params.get("print") === "1"} />
        </Panel>
      </div>
    </main>
  );
}

/** Requesting a void. The execution is somebody else's signature. */
function VoidSheet({ payment, onClose }: { payment: PaymentView; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const key = useState(() => newIdempotencyKey())[0];

  const request = useMutation({
    mutationFn: () =>
      api.post("/voids", { payment_id: payment.id, reason: reason.trim() }, { idempotencyKey: key }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["voids"] });
      onClose();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="طلب إلغاء">
      <p className="note" style={{ marginBottom: 10 }}>
        هذا طلب لا تنفيذ. التنفيذ صلاحية المدير المالي، ويصل إلى صندوق وارده بعدّاد.
      </p>
      <label className="field">
        <span className="field__label">السبب (إلزامي، ويُسجَّل باسمك)</span>
        <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} autoFocus />
      </label>
      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}
      <div className="cluster">
        <Button
          variant="danger"
          onClick={() => request.mutate()}
          disabled={reason.trim().length < 3}
          disabledReason={reason.trim().length < 3 ? "اكتب سبباً" : undefined}
          busy={request.isPending}
        >
          إرسال الطلب
        </Button>
        <Button variant="ghost" onClick={onClose}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}

/** Requesting a refund. Three stations, three signatures. */
function RefundSheet({ payment, onClose }: { payment: PaymentView; onClose: () => void }) {
  const queryClient = useQueryClient();
  const [raw, setRaw] = useState("");
  const [reason, setReason] = useState("");
  const [method, setMethod] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const key = useState(() => newIdempotencyKey())[0];

  const methods = useQuery({
    queryKey: ["payment-methods"],
    queryFn: () => api.get<PaymentMethodView[]>("/payment-methods"),
    staleTime: 300_000,
  });

  const parsed = parseInput(raw);
  const value = parsed.ok ? parsed.value : 0n;

  const request = useMutation({
    mutationFn: () =>
      api.post(
        "/refunds",
        {
          payment_id: payment.id,
          amount: Number(value),
          payment_method_id: method || methods.data?.[0]?.id,
          reason: reason.trim(),
        },
        { idempotencyKey: key },
      ),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["refunds"] });
      onClose();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="طلب استرجاع">
      {/* Three stations drawn as a route, because self-approval being blocked
          and the reversal being scoped to this payment's own allocations are
          both non-obvious. */}
      <p className="note" style={{ marginBottom: 10 }}>
        مطلوب ← موافَق ← مُرحَّل، بثلاث توقيعات. الموافقة الذاتية محجوبة. العكس مقصور على
        تخصيصات هذه الدفعة نفسها — استرجاع دفعة لا يسحب تمويلاً وفّرته دفعة أخرى.
      </p>

      <MoneyField label="المبلغ المسترجَع" value={raw} onChange={setRaw} max={amount(payment.amount)} />

      <label className="field">
        <span className="field__label">طريقة الإعادة</span>
        <select className="input" value={method} onChange={(e) => setMethod(e.target.value)}>
          {(methods.data ?? []).map((m) => (
            <option key={m.id} value={m.id}>
              {m.name_ar}
            </option>
          ))}
        </select>
      </label>

      <label className="field">
        <span className="field__label">السبب (إلزامي)</span>
        <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          onClick={() => request.mutate()}
          disabled={value <= 0n || reason.trim().length < 3}
          busy={request.isPending}
        >
          إرسال الطلب
        </Button>
        <Button variant="ghost" onClick={onClose}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
