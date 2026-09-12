import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { PaymentIntentView, PaymentProviderView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDateTime } from "@/lib/dates";
import { parseInput } from "@/lib/money";

/**
 * Electronic collection for one account.
 *
 * An intent is money on its way, not money: it becomes a payment with a
 * receipt only when the provider confirms, through the webhook or the poll
 * below. Nothing here is optimistic — the status shown is the status the
 * provider last reported, and the payment link appears only after a real
 * receipt exists.
 *
 * The amount may be left empty to charge the outstanding balance; the server
 * computes it, because making the client do it invites a stale figure.
 */
export function IntentsPanel({ accountId }: { accountId: string }) {
  const { can, reason } = useSession();
  const queryClient = useQueryClient();
  const [starting, setStarting] = useState(false);
  const [provider, setProvider] = useState("");
  const [raw, setRaw] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [key, setKey] = useState(() => newIdempotencyKey());

  const providers = useQuery({
    queryKey: ["payment-providers"],
    queryFn: () => api.get<PaymentProviderView[]>("/payment-intents/providers"),
    staleTime: 600_000,
  });

  const intents = useQuery({
    queryKey: ["payment-intents", accountId],
    queryFn: () => api.get<PaymentIntentView[]>(`/payment-intents/accounts/${accountId}`),
  });

  const initiate = useMutation({
    mutationFn: () => {
      const parsed = parseInput(raw);
      return api.post<PaymentIntentView>(
        "/payment-intents",
        {
          account_id: accountId,
          provider,
          ...(parsed.ok && parsed.value > 0n ? { amount: Number(parsed.value) } : {}),
        },
        { idempotencyKey: key },
      );
    },
    onSuccess: () => {
      setStarting(false);
      setRaw("");
      setKey(newIdempotencyKey());
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["payment-intents", accountId] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const noProviders = providers.isSuccess && (providers.data ?? []).length === 0;
  const rows = intents.data ?? [];

  return (
    <Panel
      title="الدفع الإلكتروني"
      aside={
        <Button
          size="sm"
          disabled={!can("intent.create") || noProviders}
          disabledReason={
            !can("intent.create")
              ? reason("intent.create")
              : noProviders
                ? "لا مزوّد دفع مهيأ لهذا النشر"
                : undefined
          }
          onClick={() => setStarting((s) => !s)}
        >
          {starting ? "إغلاق" : "بدء دفعة"}
        </Button>
      }
    >
      {starting && (
        <div style={{ borderBottom: "1px solid var(--rule)", paddingBottom: 12, marginBottom: 10 }}>
          <div className="cols cols--half">
            <label className="field">
              <span className="field__label">المزوّد</span>
              <select
                className="input"
                value={provider}
                onChange={(e) => setProvider(e.target.value)}
              >
                <option value="">— اختر —</option>
                {(providers.data ?? []).map((p) => (
                  <option key={p.code} value={p.code}>
                    {p.display_name}
                  </option>
                ))}
              </select>
            </label>
            <label className="field">
              <span className="field__label">المبلغ — فارغاً يعني المتبقي كله</span>
              <input
                className="input ltr"
                inputMode="numeric"
                style={{ textAlign: "end" }}
                value={raw}
                onChange={(e) => setRaw(e.target.value)}
                placeholder="يحسبه الخادم إن تُرك"
              />
            </label>
          </div>
          {refusal && (
            <div style={{ marginBottom: 10 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}
          <Button
            variant="primary"
            disabled={!provider}
            disabledReason="اختر المزوّد"
            busy={initiate.isPending}
            onClick={() => initiate.mutate()}
          >
            بدء الدفعة
          </Button>
        </div>
      )}

      {intents.isLoading && <Skeleton height={80} />}
      {intents.isSuccess && rows.length === 0 && !starting && (
        <EmptyState kind="not-yet" title="لا دفعات إلكترونية على هذا الحساب" />
      )}

      {rows.map((intent) => (
        <IntentRow key={intent.id} intent={intent} accountId={accountId} />
      ))}
    </Panel>
  );
}

function IntentRow({ intent, accountId }: { intent: PaymentIntentView; accountId: string }) {
  const queryClient = useQueryClient();

  const poll = useMutation({
    mutationFn: () => api.post<PaymentIntentView>(`/payment-intents/${intent.id}/poll`, {}),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["payment-intents", accountId] });
      void queryClient.invalidateQueries({ queryKey: ["account", accountId] });
    },
  });

  const open = intent.status === "pending" || intent.status === "initiated";

  return (
    <div className="row" style={{ alignItems: "flex-start" }}>
      <div className="grow">
        <span className="k ltr">{intent.provider}</span>{" "}
        <Money value={intent.amount} tone="plain" />
        {intent.provider_ref && (
          <span className="label">
            {" "}
            · مرجع <span className="ltr num">{intent.provider_ref}</span>
          </span>
        )}
        <div className="note">
          بدأت {formatDateTime(intent.created_at)}
          {intent.expires_at && <> · تنتهي {formatDateTime(intent.expires_at)}</>}
        </div>
        {/* The counter instruction, when there is no redirect. */}
        {intent.instruction && <div className="note">{intent.instruction}</div>}
      </div>
      <IntentStatus status={intent.status} />
      {intent.payment_id && (
        <Link className="btn btn--sm" to={`/payments/${intent.payment_id}`}>
          الوصل
        </Link>
      )}
      {open && (
        <Button size="sm" busy={poll.isPending} onClick={() => poll.mutate()} title="اسأل المزوّد الآن">
          استعلام
        </Button>
      )}
    </div>
  );
}

function IntentStatus({ status }: { status: string }) {
  switch (status) {
    case "pending":
    case "initiated":
      return <Chip tone="pending">بانتظار المزوّد</Chip>;
    case "confirmed":
    case "completed":
      return <Chip tone="live">تأكدت — صار لها وصل</Chip>;
    case "failed":
      return <Chip tone="void">فشلت</Chip>;
    case "expired":
      return <Chip tone="muted">انتهت</Chip>;
    case "cancelled":
      return <Chip tone="muted">أُلغيت</Chip>;
    default:
      return <Chip tone="muted">{status}</Chip>;
  }
}
