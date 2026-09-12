import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { RefundView } from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { elapsed, formatDateTime } from "@/lib/dates";

/**
 * The refund inbox — §09.
 *
 * Three stations with three signatures, drawn as a route rather than as a
 * status word: requested → approved → posted. Two of its rules are not
 * obvious, so the screen states them: self-approval is blocked, and the
 * reversal is scoped to the refunded payment's own allocations — unwinding
 * "the newest installments" across the account would let a refund of payment A
 * strip the funding payment B provided.
 */
export function RefundInboxScreen() {
  const { can, reason, user } = useSession();

  const pending = useQuery({
    queryKey: ["refunds", "pending"],
    queryFn: () => api.get<RefundView[]>("/refunds/pending"),
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
          <EmptyState kind="no-results" title="تعذّر جلب الاسترجاعات" />
        )}
      </main>
    );
  }

  const rows = pending.data ?? [];

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">الاسترجاعات</h1>
        {rows.length > 0 && <Chip tone="pending">{rows.length} معلّقة</Chip>}
      </div>

      <div className="callout callout--note" style={{ marginTop: 10 }}>
        <b>خط السير:</b> مطلوب ← موافَق ← مُرحَّل. ثلاث محطات بثلاث توقيعات. الموافقة الذاتية
        محجوبة، والعكس مقصور على تخصيصات الدفعة نفسها — استرجاع دفعة لا يسحب تمويلاً وفّرته دفعة
        أخرى.
      </div>

      {rows.length === 0 ? (
        <EmptyState kind="clean" title="لا استرجاعات معلّقة" />
      ) : (
        <div className="stack" style={{ marginTop: 12 }}>
          {rows.map((refund) => (
            <RefundCard
              key={refund.id}
              refund={refund}
              currentUserId={user?.id ?? ""}
              canApprove={can("refund.approve")}
              approveReason={reason("refund.approve")}
              canPost={can("refund.post")}
              postReason={reason("refund.post")}
            />
          ))}
        </div>
      )}
    </main>
  );
}

function RefundCard({
  refund,
  canApprove,
  approveReason,
  canPost,
  postReason,
}: {
  refund: RefundView;
  currentUserId: string;
  canApprove: boolean;
  approveReason: string | undefined;
  canPost: boolean;
  postReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [rejecting, setRejecting] = useState(false);
  const [rejectReason, setRejectReason] = useState("");
  const postKey = useState(() => newIdempotencyKey())[0];

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["refunds"] });
  };
  const onError = (error: unknown) => {
    if (isRefusal(error)) setRefusal(error);
  };

  const approve = useMutation({
    mutationFn: () => api.post(`/refunds/${refund.id}/approve`, {}),
    onSuccess: invalidate,
    onError,
  });

  const reject = useMutation({
    mutationFn: () => api.post(`/refunds/${refund.id}/reject`, { reason: rejectReason.trim() }),
    onSuccess: () => {
      setRejecting(false);
      invalidate();
    },
    onError,
  });

  const post = useMutation({
    mutationFn: () => api.post(`/refunds/${refund.id}/post`, {}, { idempotencyKey: postKey }),
    onSuccess: invalidate,
    onError,
  });

  const stations: { id: string; label: string }[] = [
    { id: "requested", label: "مطلوب" },
    { id: "approved", label: "موافَق" },
    { id: "posted", label: "مُرحَّل" },
  ];
  const reached = stations.findIndex((s) => s.id === refund.status);

  return (
    <Panel
      title={
        <>
          استرجاع{" "}
          <span className="ltr num">{refund.refund_no ?? "—"}</span>{" "}
          <StateChip entity="refund" status={refund.status} />
        </>
      }
      aside={<span className="label">منذ {elapsed(refund.requested_at)}</span>}
    >
      {/* The route, drawn. */}
      <div className="cluster" style={{ marginBottom: 12 }}>
        {stations.map((station, index) => (
          <span
            key={station.id}
            className="sheet__stage"
            data-state={
              index < reached ? "done" : index === reached ? "current" : undefined
            }
          >
            {index + 1} · {station.label}
          </span>
        ))}
      </div>

      <div className="cols cols--half">
        <div>
          <Row label="المبلغ">
            <Money value={refund.amount} size="big" tone="plain" />
          </Row>
          <Row label="السبب">«{refund.reason}»</Row>
        </div>
        <div>
          <Row label="طُلب">
            <span className="num">{formatDateTime(refund.requested_at)}</span>
          </Row>
          <Row label="الوصل الأصل">
            <Link className="ltr num" to={`/payments/${refund.payment_id}`}>
              فتح الوصل
            </Link>
          </Row>
          <Row label="الحساب">
            <Link className="ltr num" to={`/accounts/${refund.account_id}`}>
              فتح الحساب
            </Link>
          </Row>
        </div>
      </div>

      {refusal && (
        <div style={{ marginTop: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      {rejecting && (
        <label className="field" style={{ marginTop: 10 }}>
          <span className="field__label">سبب الرفض (إلزامي)</span>
          <input
            className="input"
            autoFocus
            value={rejectReason}
            onChange={(e) => setRejectReason(e.target.value)}
          />
        </label>
      )}

      <div className="cluster" style={{ marginTop: 12 }}>
        {refund.status === "requested" && (
          <>
            <Button
              variant="primary"
              disabled={!canApprove}
              disabledReason={approveReason}
              busy={approve.isPending}
              onClick={() => approve.mutate()}
            >
              موافقة
            </Button>
            {rejecting ? (
              <>
                <Button
                  variant="danger"
                  disabled={rejectReason.trim().length < 3}
                  busy={reject.isPending}
                  onClick={() => reject.mutate()}
                >
                  تأكيد الرفض
                </Button>
                <Button variant="ghost" onClick={() => setRejecting(false)}>
                  تراجع
                </Button>
              </>
            ) : (
              <Button
                disabled={!canApprove}
                disabledReason={approveReason}
                onClick={() => setRejecting(true)}
              >
                رفض بسبب
              </Button>
            )}
          </>
        )}

        {refund.status === "approved" && (
          <Button
            variant="primary"
            disabled={!canPost}
            disabledReason={postReason}
            busy={post.isPending}
            onClick={() => post.mutate()}
          >
            ترحيل الاسترجاع
          </Button>
        )}
      </div>
    </Panel>
  );
}
