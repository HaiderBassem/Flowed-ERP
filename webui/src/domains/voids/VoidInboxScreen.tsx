import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { PaymentView, VoidRequestView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { elapsed, formatDateTime } from "@/lib/dates";

/**
 * The void inbox — §07 pattern four.
 *
 * An inbox with a counter rather than a button on a document page, because a
 * pending void is waiting on *another person*: leaving it to be found by
 * whoever happens to open the payment is how a request sleeps for a week while
 * the student holds a receipt for an unresolved transaction.
 *
 * The card carries what the decision needs without opening another screen, and
 * two of those figures are the whole reason the void register exists: the
 * **time gap** between the collection and the request, and whether the request
 * crosses a shift. A void across days empties a drawer that has already been
 * counted, and the rule is that such a case is a refund, not a void — so the
 * card says so rather than leaving the approver to remember it.
 */
export function VoidInboxScreen() {
  const { can, reason } = useSession();

  const pending = useQuery({
    queryKey: ["voids", "pending"],
    queryFn: () => api.get<VoidRequestView[]>("/voids/pending"),
    refetchInterval: 60_000,
  });

  if (pending.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="30%" />
        <div style={{ height: 12 }} />
        <Skeleton height={200} />
      </main>
    );
  }

  if (pending.isError) {
    return (
      <main className="screen">
        {isRefusal(pending.error) ? (
          <RefusalPanel refusal={pending.error} onRetry={() => void pending.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر جلب الطلبات" />
        )}
      </main>
    );
  }

  const rows = pending.data ?? [];
  const oldest = rows.reduce<string | null>(
    (acc, r) => (acc === null || r.requested_at < acc ? r.requested_at : acc),
    null,
  );

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">طلبات الإلغاء</h1>
        {rows.length > 0 && <Chip tone="pending">{rows.length} معلّقة</Chip>}
        <span className="grow" />
        {oldest && <span className="label">الأقدم منذ {elapsed(oldest)}</span>}
      </div>

      {rows.length === 0 ? (
        <EmptyState kind="clean" title="لا طلبات معلّقة" detail="لا شيء ينتظر توقيعك." />
      ) : (
        <div className="stack" style={{ marginTop: 12 }}>
          {rows.map((request) => (
            <VoidCard
              key={request.id}
              request={request}
              canExecute={can("payment.void.execute")}
              executeReason={reason("payment.void.execute")}
            />
          ))}
        </div>
      )}
    </main>
  );
}

function VoidCard({
  request,
  canExecute,
  executeReason,
}: {
  request: VoidRequestView;
  canExecute: boolean;
  executeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [understood, setUnderstood] = useState(false);
  const key = useState(() => newIdempotencyKey())[0];

  // The payment the request is about. The card is useless without the amount
  // and the collection time, and making the approver open another screen to
  // find them is how approvals become rubber stamps.
  const payment = useQuery({
    queryKey: ["payment", request.payment_id],
    queryFn: () => api.get<PaymentView>(`/payments/${request.payment_id}`),
  });

  const execute = useMutation({
    mutationFn: () => api.post(`/voids/${request.id}/execute`, {}, { idempotencyKey: key }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["voids"] });
      void queryClient.invalidateQueries({ queryKey: ["payment", request.payment_id] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const collectedAt = payment.data?.posted_at ?? payment.data?.paid_at;
  const gap = collectedAt ? elapsed(collectedAt, request.requested_at) : null;

  // Crossing a day is the signal worth surfacing; the rule says such a case
  // should have been a refund.
  const crossesDay =
    collectedAt !== undefined &&
    collectedAt !== null &&
    new Date(collectedAt).toDateString() !== new Date(request.requested_at).toDateString();

  return (
    <Panel
      title={
        <>
          وصل{" "}
          <Link className="ltr num" to={`/payments/${request.payment_id}`}>
            {payment.data?.receipt_no ?? "…"}
          </Link>
        </>
      }
      aside={<Chip tone="pending">ينتظر تنفيذك</Chip>}
    >
      <div className="cols cols--half">
        <div>
          <Row label="المبلغ">
            {payment.data ? (
              <Money value={payment.data.amount} size="big" tone="plain" />
            ) : (
              <Skeleton height={20} width={90} />
            )}
          </Row>
          <Row label="رُحِّل">
            <span className="num">{formatDateTime(collectedAt ?? null)}</span>
          </Row>
          <Row label="طُلب الإلغاء">
            <span className="num">{formatDateTime(request.requested_at)}</span>
          </Row>
          <Row label="الفجوة">
            <span className={`num${crossesDay ? " money--neg" : ""}`}>
              {gap ?? "—"}
              {crossesDay && " · عبر الأيام"}
            </span>
          </Row>
        </div>
        <div>
          <Row label="السبب">«{request.reason}»</Row>
          <Row label="حالة الوصل">
            {payment.data?.status === "voided" ? (
              <Chip tone="void">ملغى أصلاً</Chip>
            ) : (
              <Chip tone="live">مُرحَّل</Chip>
            )}
          </Row>
        </div>
      </div>

      {crossesDay && (
        <div className="refusal refusal--pending" style={{ marginTop: 10 }}>
          <p className="refusal__title">تنبيه رقابي</p>
          <p className="refusal__body" style={{ margin: 0 }}>
            الطلب بعد يوم القبض. الإلغاء عبر الأيام يُصفّي درجاً سبق عدّه — القاعدة تقول إن هذه
            الحالة استرجاع لا إلغاء.
          </p>
        </div>
      )}

      {refusal && (
        <div style={{ marginTop: 10 }}>
          <RefusalPanel
            refusal={refusal}
            actions={[
              {
                label: "فتح الوصل",
                onClick: () => {
                  window.location.href = `/app/payments/${request.payment_id}`;
                },
              },
            ]}
          />
        </div>
      )}

      {/* Consequence before confirmation: what changes, by how much, and that
          it is one-way. "Are you sure?" asks about mood; this asks about
          money. */}
      {payment.data && payment.data.status !== "voided" && (
        <div className="callout callout--warn" style={{ marginTop: 12 }}>
          <b>ما سيحدث عند التنفيذ:</b>
          <ul style={{ margin: "6px 0 0", paddingInlineStart: 18 }}>
            <li>
              الوصل <span className="ltr num">{payment.data.receipt_no ?? ""}</span> يُبطل نهائياً
              ويبقى ظاهراً مشطوباً — لا حذف.
            </li>
            <li>
              مبلغ <Money value={payment.data.amount} tone="plain" /> يعود ديناً قائماً على
              أقساط الحساب التي كان يسددها.
            </li>
            <li>لا تراجع عن الإلغاء — التصحيح بعده يكون بقبض جديد بوصل جديد.</li>
          </ul>
          <label className="cluster" style={{ marginTop: 10, cursor: "pointer" }}>
            <input
              type="checkbox"
              checked={understood}
              onChange={(e) => setUnderstood(e.target.checked)}
            />
            <b>أفهم الأثر المالي، والتنفيذ يُسجَّل باسمي</b>
          </label>
        </div>
      )}

      <div className="cluster" style={{ marginTop: 12 }}>
        <Button
          variant="danger"
          disabled={!canExecute || payment.data?.status === "voided" || !understood}
          disabledReason={
            !canExecute
              ? executeReason
              : payment.data?.status === "voided"
                ? "الوصل ملغى أصلاً"
                : !understood
                  ? "أقرّ بالأثر المالي أولاً"
                  : undefined
          }
          busy={execute.isPending}
          onClick={() => execute.mutate()}
        >
          تنفيذ الإلغاء
        </Button>
        <span className="grow" />
        <Link className="btn btn--ghost" to={`/payments/${request.payment_id}`}>
          فتح الوصل
        </Link>
      </div>

      <p className="note" style={{ marginTop: 8 }}>
        الإلغاء مرفوض إذا كان على الوصل استرجاع مُرحَّل — إلغاء وصل مليون بعد استرجاع 400,000
        يعني صرف 1,400,000 مقابل مليون مقبوضة. الرفض حينها يشرح البديل.
      </p>
    </Panel>
  );
}
