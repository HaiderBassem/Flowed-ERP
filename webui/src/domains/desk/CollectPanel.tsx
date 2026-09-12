import { useEffect, useMemo, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { Link } from "react-router-dom";

import { api, newIdempotencyKey } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type {
  AccountDetailView,
  PaymentMethodView,
  RecordPaymentResponse,
  StudentView,
} from "@/api/types";
import { Chip } from "@/components/Chip";
import { Money, MoneyField } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, Kbd, Panel, Row } from "@/components/primitives";
import { amount, format, parseInput, spellArabic, type Amount } from "@/lib/money";
import { previewAllocation } from "./allocation";

/**
 * The collection panel.
 *
 * §03 principle 07 governs this component completely: money does not move
 * optimistically. The state machine is
 *
 *   ready → posting (control locked, explicit state) → posted, with a receipt
 *   number the server issued
 *
 * and a receipt number never appears before the server grants it, because it
 * comes from a locked counter at the moment of posting. The failure that rule
 * prevents is printing a slip for a transaction that rolled back.
 *
 * The idempotency key is minted when the panel opens for an account, not when
 * the button is pressed, and it is kept across retries of that attempt. That
 * is what makes "retry" a safe instruction to give a cashier whose network
 * dropped mid-post.
 */

type Phase =
  | { name: "ready" }
  | { name: "posting" }
  | { name: "posted"; result: RecordPaymentResponse }
  | { name: "refused"; refusal: Refusal };

export function CollectPanel({
  account,
  student,
  canCollect,
  collectReason,
  amountRef,
  onPosted,
}: {
  account: AccountDetailView;
  student: StudentView;
  canCollect: boolean;
  collectReason: string | undefined;
  amountRef?: React.RefObject<HTMLInputElement | null>;
  onPosted: (result: RecordPaymentResponse) => void;
}) {
  const queryClient = useQueryClient();
  const [raw, setRaw] = useState("");
  const [method, setMethod] = useState<string>("");
  const [reference, setReference] = useState("");
  const [phase, setPhase] = useState<Phase>({ name: "ready" });
  const [confirmedDistinct, setConfirmedDistinct] = useState(false);

  // Minted per attempt, held in a ref so a re-render cannot mint a second one.
  const idempotencyKey = useRef(newIdempotencyKey());

  const methods = useQuery({
    queryKey: ["payment-methods"],
    queryFn: () => api.get<PaymentMethodView[]>("/payment-methods"),
    staleTime: 300_000,
  });

  useEffect(() => {
    if (!method && methods.data && methods.data.length > 0) {
      setMethod(methods.data.find((m) => m.is_cash)?.id ?? methods.data[0]!.id);
    }
  }, [methods.data, method]);

  // A new account means a new attempt: new key, cleared field, cleared state.
  useEffect(() => {
    idempotencyKey.current = newIdempotencyKey();
    setRaw("");
    setPhase({ name: "ready" });
    setConfirmedDistinct(false);
  }, [account.account.id]);

  const parsed = useMemo(() => parseInput(raw), [raw]);
  const value: Amount = parsed.ok ? parsed.value : (0n as Amount);
  const remaining = amount(account.account.remaining);

  const preview = useMemo(
    () => previewAllocation(account.installments, value),
    [account.installments, value],
  );

  const post = useMutation({
    mutationFn: async () => {
      return api.post<RecordPaymentResponse>(
        "/payments",
        {
          account_id: account.account.id,
          amount: Number(value),
          payment_method_id: method,
          ...(reference.trim() ? { method_reference: reference.trim() } : {}),
          ...(confirmedDistinct ? { confirmed_distinct: true } : {}),
        },
        { idempotencyKey: idempotencyKey.current },
      );
    },
    onMutate: () => setPhase({ name: "posting" }),
    onSuccess: (result) => {
      setPhase({ name: "posted", result });
      onPosted(result);
      void queryClient.invalidateQueries({ queryKey: ["account", account.account.id] });
      void queryClient.invalidateQueries({ queryKey: ["student-accounts", student.id] });
      void queryClient.invalidateQueries({ queryKey: ["cashier-session"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setPhase({ name: "refused", refusal: error });
      else setPhase({ name: "ready" });
    },
  });

  const posting = phase.name === "posting";
  const ready = parsed.ok && value > 0n && Boolean(method) && canCollect && !posting;

  const submit = () => {
    if (!ready) return;
    post.mutate();
  };

  // F9 posts and prints. Bound at the panel rather than globally so it cannot
  // fire while the operator is somewhere else on the screen.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "F9") {
        event.preventDefault();
        submit();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  if (phase.name === "posted") {
    return <PostedPanel result={phase.result} onNext={() => {
      idempotencyKey.current = newIdempotencyKey();
      setRaw("");
      setPhase({ name: "ready" });
    }} />;
  }

  return (
    <Panel
      title="قبض"
      aside={<span className="label">حساب هذا العام</span>}
    >
      <MoneyField
        label="المبلغ (دينار)"
        value={raw}
        onChange={setRaw}
        max={remaining}
        disabled={!canCollect || posting}
        {...(amountRef ? { inputRef: amountRef } : {})}
        onEnter={submit}
      />

      <div className="field">
        <span className="field__label">طريقة الدفع</span>
        <div className="cluster">
          {(methods.data ?? []).map((m) => (
            <button
              key={m.id}
              type="button"
              className={`btn${method === m.id ? " btn--primary" : ""}`}
              onClick={() => setMethod(m.id)}
              disabled={posting}
            >
              {m.name_ar}
            </button>
          ))}
        </div>
      </div>

      {/* A non-cash method carries a reference; cash does not, and asking for
          one would be a field the cashier learns to leave blank. */}
      {methods.data?.find((m) => m.id === method && !m.is_cash) && (
        <label className="field">
          <span className="field__label">رقم الإشعار / المرجع</span>
          <input
            className="input"
            value={reference}
            onChange={(e) => setReference(e.target.value)}
            disabled={posting}
          />
        </label>
      )}

      {value > 0n && (
        <div className="field">
          <span className="field__label">التوزيع (الأقدم استحقاقاً أولاً)</span>
          {preview.lines.length === 0 && (
            <p className="note">لا أقساط مستحقة — المبلغ كله يتحوّل إلى رصيد دائن.</p>
          )}
          {preview.lines.map((line) => (
            <Row key={line.installment.id}>
              <span className="grow">
                القسط <span className="num">{line.installment.number}</span>
                {line.installment.is_overdue && (
                  <>
                    {" "}
                    <Chip tone="void">متأخر</Chip>
                  </>
                )}
              </span>
              <Money value={line.applied} tone="positive" />
            </Row>
          ))}

          {preview.settlesAccount && preview.surplus === 0n && (
            <Row className="waterfall__line--frozen">
              <span className="grow">
                <b>يُسدَّد الحساب بالكامل</b>
              </span>
              <Money value={value} tone="positive" />
            </Row>
          )}

          {/* Surplus is shown before posting, as a credit balance — §08 is
              explicit that it must not be discovered after the print. */}
          {preview.surplus > 0n && (
            <Row className="waterfall__line--frozen">
              <span className="grow">
                <b>فائض يتحوّل إلى رصيد دائن</b>
                <span className="label"> — يبقى على الحساب ولا يُعاد نقداً</span>
              </span>
              <Money value={preview.surplus} tone="positive" />
            </Row>
          )}

          <p className="note" style={{ marginTop: 6 }}>
            معاينة للقاعدة الافتراضية. التوزيع المعتمد هو ما يعيده الخادم عند الترحيل، ويُعرض بعده.
          </p>
        </div>
      )}

      {/* The written amount, before committing: this is what will be printed,
          and it is what makes the figure hard to alter afterwards. */}
      {value >= 1_000_000n && (
        <div className="callout callout--note" style={{ marginBottom: 12 }}>
          <span className="label">تفقيط ما سيُطبع</span>
          <div>{spellArabic(value)}</div>
        </div>
      )}

      {phase.name === "refused" && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel
            refusal={phase.refusal}
            {...(phase.refusal.outcomeUnknown || phase.refusal.kind === "rate_limited"
              ? { onRetry: () => post.mutate() }
              : {})}
            {...(phase.refusal.code.includes("duplicate") ||
            phase.refusal.code.includes("near_duplicate")
              ? {
                  actions: [
                    {
                      label: "نعم، عملية منفصلة",
                      primary: true,
                      onClick: () => {
                        setConfirmedDistinct(true);
                        // A fresh key: this is a genuinely different collection,
                        // and replaying the old key would return the first one.
                        idempotencyKey.current = newIdempotencyKey();
                        post.mutate();
                      },
                    },
                  ],
                }
              : {})}
          />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          size="lg"
          onClick={submit}
          disabled={!ready}
          disabledReason={
            !canCollect
              ? collectReason
              : !parsed.ok || value === 0n
                ? "أدخل مبلغاً أولاً"
                : undefined
          }
          busy={posting}
        >
          {posting ? "قيد الترحيل…" : "ترحيل وطباعة"} <Kbd>F9</Kbd>
        </Button>
      </div>

      <p className="note" style={{ marginTop: 8 }}>
        رقم الوصل يُمنح لحظة الترحيل من عدّاد الشبّاك — لا يُعرض قبله. هذه المحاولة محمية من
        التكرار: إعادة الضغط أو انقطاع الشبكة لا يقبضان مرتين.
      </p>
    </Panel>
  );
}

/**
 * After posting.
 *
 * A replayed key is announced rather than reprinted: the terminal has already
 * produced this slip once, and §01 asks for "إعادة" in place of a second
 * original.
 */
function PostedPanel({
  result,
  onNext,
}: {
  result: RecordPaymentResponse;
  onNext: () => void;
}) {
  return (
    <Panel
      title={
        <>
          {result.duplicate ? "هذه إعادة — لم يُقبض مرة ثانية" : "رُحِّلت"}{" "}
          {result.duplicate && <Chip tone="frozen">إعادة</Chip>}
        </>
      }
    >
      <Row label="رقم الوصل">
        <span className="ltr num">
          <b>{result.payment.receipt_no ?? "—"}</b>
        </span>
      </Row>
      <Row label="المبلغ">
        <Money value={result.payment.amount} size="big" tone="positive" />
      </Row>
      <Row label="المتبقي على الحساب">
        <Money value={result.remaining} tone="plain" />
      </Row>
      {amount(result.credit_amount) > 0n && (
        <Row label="رصيد دائن">
          <Money value={result.credit_amount} tone="positive" />
        </Row>
      )}

      <div className="cluster" style={{ marginTop: 12 }}>
        <Link className="btn btn--primary" to={`/payments/${result.payment.id}?print=1`}>
          طباعة الوصل
        </Link>
        <Link className="btn" to={`/payments/${result.payment.id}`}>
          فتح الوصل
        </Link>
        <span className="grow" />
        <Button onClick={onNext}>قبض آخر</Button>
      </div>

      <p className="note" style={{ marginTop: 8 }}>
        التوزيع المعتمد: {result.allocations.length} قسط ·{" "}
        {result.allocations.map((a) => format(amount(a.amount))).join(" · ")}
      </p>
    </Panel>
  );
}
