import { useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type {
  AccountDetailView,
  AccountView,
  EnrollmentView,
  Page,
  PaymentView,
  RecordPaymentResponse,
  StudentView,
  YearView,
} from "@/api/types";
import { Chip, StateChip } from "@/components/Chip";
import { Money } from "@/components/Money";
import { EmptyState, Kbd, Panel, Row } from "@/components/primitives";
import { InstallmentTable } from "@/domains/accounts/InstallmentTable";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { amount } from "@/lib/money";
import { fold } from "@/lib/text";
import { CollectPanel } from "./CollectPanel";

/**
 * The collection window — §08.
 *
 * One screen that is never left. Search, the student's years, the account, the
 * collection and the print are all in one fixed layout, operable end to end
 * from the keyboard, laid out for 1366×768 first and widened from there.
 *
 * Three things on it are structure rather than decoration:
 *
 *   - a **column of the student's years**, not one balance. The student at the
 *     window may owe two years, each with its own account, and each needs its
 *     own receipt because one payment belongs to one account.
 *   - the **allocation before posting**, so the cashier sees where the money
 *     lands and whether there is a surplus.
 *   - **voided receipts stay visible**, struck through, in the shift list.
 *     Hiding them hides precisely the pattern the void log exists to expose.
 *
 * There is no edit control and no delete control anywhere on it. Their absence
 * is the design: the legitimate corrections are void, refund and adjustment,
 * each of which creates a new visible document.
 */
export function DeskScreen() {
  const [params, setParams] = useSearchParams();
  const { can, reason } = useSession();
  const { activeYear, years } = useWorkingContext();

  const [term, setTerm] = useState(params.get("student") ?? "");
  const [selectedStudent, setSelectedStudent] = useState<string | null>(null);
  const [selectedAccount, setSelectedAccount] = useState<string | null>(null);
  const [justPosted, setJustPosted] = useState<RecordPaymentResponse[]>([]);

  const searchRef = useRef<HTMLInputElement>(null);
  const amountRef = useRef<HTMLInputElement>(null);

  // Collecting no longer waits on an open shift: there are no shifts. What can
  // still stop it is a year whose books are shut, and that refusal has to be
  // stated rather than shown as a disabled button with no reason.
  const canCollect = can("payment.record");
  const collectReason = !canCollect
    ? reason("payment.record")
    : activeYear && !activeYear.accepts_financial_posting
      ? `السنة ${activeYear.code} لا تقبل الترحيل المالي`
      : undefined;

  /* --------------------------------------------------------------- search */

  const debounced = useDebounced(term, 220);
  const results = useQuery({
    queryKey: ["students", "desk", debounced],
    queryFn: () => api.get<Page<StudentView>>("/students", { query: { q: debounced, limit: 8 } }),
    enabled: debounced.trim().length >= 2,
  });

  const found = results.data?.data ?? [];

  // An exact student number match opens straight through: at a window in peak
  // hour, a list of one is a keystroke nobody should have to spend.
  useEffect(() => {
    if (found.length === 1 && found[0]!.student_no === debounced.trim()) {
      setSelectedStudent(found[0]!.id);
    }
  }, [found, debounced]);

  /* ------------------------------------------------------- student & years */

  const student = useQuery({
    queryKey: ["student", selectedStudent],
    queryFn: () => api.get<StudentView>(`/students/${selectedStudent}`),
    enabled: Boolean(selectedStudent),
  });

  const accounts = useQuery({
    queryKey: ["student-accounts", selectedStudent],
    queryFn: () => api.get<AccountView[]>(`/students/${selectedStudent}/accounts`),
    enabled: Boolean(selectedStudent),
  });

  const enrollments = useQuery({
    queryKey: ["student-enrollments", selectedStudent],
    queryFn: () => api.get<EnrollmentView[]>(`/students/${selectedStudent}/enrollments`),
    enabled: Boolean(selectedStudent),
  });

  const collectable = useMemo(
    () => (accounts.data ?? []).filter((a) => a.status !== "cancelled"),
    [accounts.data],
  );

  useEffect(() => {
    if (collectable.length === 0) {
      setSelectedAccount(null);
      return;
    }
    if (!selectedAccount || !collectable.some((a) => a.id === selectedAccount)) {
      // Default to the active year's account if the student has one, otherwise
      // the one that still owes money.
      const forActiveYear = collectable.find((a) => a.academic_year_id === activeYear?.id);
      const owing = collectable.find((a) => amount(a.remaining) > 0n);
      setSelectedAccount((forActiveYear ?? owing ?? collectable[0]!).id);
    }
  }, [collectable, selectedAccount, activeYear]);

  const account = useQuery({
    queryKey: ["account", selectedAccount],
    queryFn: () => api.get<AccountDetailView>(`/accounts/${selectedAccount}`),
    enabled: Boolean(selectedAccount),
  });

  /* ------------------------------------------------------------- keyboard */

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      const target = event.target as HTMLElement | null;
      const typing =
        target &&
        (target.tagName === "INPUT" || target.tagName === "TEXTAREA" || target.isContentEditable);

      // Physical codes throughout (event.code): the cashier's layout is
      // Arabic, where the key that engraves "/" reports "ظ" and "A" reports
      // "ش". A shortcut compared against the character dies with the layout;
      // the key position does not.
      if (event.code === "Slash" && !typing) {
        event.preventDefault();
        searchRef.current?.focus();
        searchRef.current?.select();
        return;
      }
      if (event.key === "Escape") {
        if (typing && target === searchRef.current) return;
        event.preventDefault();
        clear();
        return;
      }
      if (!typing && event.code === "KeyA") {
        event.preventDefault();
        amountRef.current?.focus();
        amountRef.current?.select();
        return;
      }
      const digit = /^(?:Digit|Numpad)([1-9])$/.exec(event.code);
      if (!typing && digit) {
        const index = Number(digit[1]) - 1;
        const target2 = collectable[index];
        if (target2) {
          event.preventDefault();
          setSelectedAccount(target2.id);
        }
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  });

  const clear = () => {
    // Confirmed only when there is something to lose. A confirmation on an
    // empty screen is a confirmation nobody reads by the third hour.
    if (amountRef.current?.value && amountRef.current.value !== "0") {
      if (!window.confirm("هناك مبلغ مكتوب لم يُرحَّل. مسح الشاشة؟")) return;
    }
    setSelectedStudent(null);
    setSelectedAccount(null);
    setTerm("");
    setParams(new URLSearchParams(), { replace: true });
    searchRef.current?.focus();
  };

  const yearOf = (id: string): YearView | undefined => years.find((y) => y.id === id);

  return (
    <main className="screen screen--wide">
      <div className="cols cols--desk">
        {/* ------------------------------------------------------- left */}
        <div className="stack">
          <Panel>
            <label className="visually-hidden" htmlFor="desk-search">
              بحث عن طالب
            </label>
            <div className="cluster">
              <Kbd>/</Kbd>
              <input
                id="desk-search"
                ref={searchRef}
                className="input grow"
                autoFocus
                autoComplete="off"
                placeholder="رقم جامعي أو اسم أو آخر 4 أرقام هاتف…"
                value={term}
                onChange={(e) => {
                  setTerm(e.target.value);
                  setSelectedStudent(null);
                }}
                onKeyDown={(e) => {
                  if (e.key === "Enter" && found[0]) {
                    e.preventDefault();
                    setSelectedStudent(found[0].id);
                  }
                }}
              />
            </div>

            {/* Showing the fold: searching فاطمه and getting فاطمة otherwise
                reads as the system having fetched a different person. */}
            {debounced.trim().length >= 2 && fold(debounced) !== debounced.trim() && (
              <p className="note" style={{ marginTop: 6 }}>
                طُبِّع البحث إلى <span className="ltr">{fold(debounced)}</span> — النتائج تشمل
                الصيغتين
              </p>
            )}

            {!selectedStudent && debounced.trim().length >= 2 && (
              <div style={{ marginTop: 8 }}>
                {found.length === 0 && !results.isFetching && (
                  <EmptyState kind="no-results" title="لا نتائج لهذا البحث" />
                )}
                {found.map((s) => (
                  <button
                    key={s.id}
                    type="button"
                    className="palette__row"
                    style={{ borderRadius: 6 }}
                    onClick={() => setSelectedStudent(s.id)}
                  >
                    <span className="grow">
                      <b>{s.full_name}</b>{" "}
                      {/* Permanent column, not a secondary field: two names
                          identical after folding are told apart by it. */}
                      <span className="label">— الأم: {s.mother_name}</span>
                    </span>
                    <span className="num">{s.student_no}</span>
                  </button>
                ))}
              </div>
            )}
          </Panel>

          {student.data && (
            <Panel
              title={
                <>
                  {student.data.full_name} <span className="label">· الأم: {student.data.mother_name}</span>
                </>
              }
              aside={
                <Link className="num" to={`/students/${student.data.id}`}>
                  {student.data.student_no}
                </Link>
              }
            >
              {collectable.length === 0 && (
                <EmptyState
                  kind="not-yet"
                  title="لا حساب مالي لهذا الطالب"
                  detail="التسجيل وحده لا يولّد حساباً — التوليد أمر منفصل يقوم به المدير المالي."
                />
              )}

              {/* A column of years, not one balance. */}
              {collectable.map((a, index) => {
                const year = yearOf(a.academic_year_id);
                const enrollment = (enrollments.data ?? []).find(
                  (e) => e.id === a.enrollment_id,
                );
                const selected = a.id === selectedAccount;
                return (
                  <button
                    key={a.id}
                    type="button"
                    className="row"
                    style={{
                      width: "100%",
                      background: selected ? "var(--accent-soft)" : "transparent",
                      border: 0,
                      borderRadius: 6,
                      cursor: "pointer",
                      textAlign: "start",
                      paddingInline: 8,
                    }}
                    onClick={() => setSelectedAccount(a.id)}
                    aria-pressed={selected}
                  >
                    <Kbd>{index + 1}</Kbd>
                    {year && <StateChip entity="year" status={year.status} />}
                    <span className="grow">
                      <b>{year?.code ?? "—"}</b>
                      {enrollment && (
                        <span className="label">
                          {" "}
                          · المرحلة <span className="num">{enrollment.stage}</span>
                        </span>
                      )}
                      {year && !year.accepts_financial_posting && (
                        <span className="label"> — سنة مغلقة مالياً</span>
                      )}
                    </span>
                    <span className="label">المتبقي</span>
                    <Money
                      value={a.remaining}
                      tone={amount(a.remaining) > 0n ? "auto" : "plain"}
                    />
                  </button>
                );
              })}

              {collectable.length > 1 && (
                <p className="note" style={{ marginTop: 8 }}>
                  دين السنة المغلقة يبقى على حسابها. قبضه مشروع ويُرحَّل بوصل السنة الحالية —{" "}
                  <b>ويحتاج وصلاً منفصلاً</b>، لأن الدفعة الواحدة تخص حساباً واحداً.
                </p>
              )}
            </Panel>
          )}

          {account.data && (
            <Panel
              title={<>أقساط حساب {yearOf(account.data.account.academic_year_id)?.code ?? ""}</>}
              aside={
                <span className="label">
                  الصافي الفعّال <Money value={account.data.account.effective_net} tone="plain" />
                </span>
              }
              flush
            >
              <InstallmentTable installments={account.data.installments} />
            </Panel>
          )}
        </div>

        {/* ------------------------------------------------------ right */}
        <div className="stack">
          {collectReason && (
            <div className="callout callout--warn">
              <b>القبض متعذّر الآن</b>
              <p style={{ margin: "4px 0 0" }}>{collectReason}</p>
            </div>
          )}

          {account.data && student.data ? (
            <CollectPanel
              account={account.data}
              student={student.data}
              canCollect={canCollect && !collectReason}
              collectReason={collectReason}
              amountRef={amountRef}
              onPosted={(result) => setJustPosted((prev) => [result, ...prev])}
            />
          ) : (
            <Panel title="قبض">
              <EmptyState
                kind="not-yet"
                title="اختر طالباً أولاً"
                detail={
                  <span>
                    اضغط <Kbd>/</Kbd> للبحث، ثم <Kbd>Enter</Kbd> لفتح أول نتيجة.
                  </span>
                }
              />
            </Panel>
          )}

          <ShiftReceipts justPosted={justPosted} />
        </div>
      </div>
    </main>
  );
}

/**
 * What this browser session has collected.
 *
 * Voided receipts stay in the list, struck through and still numbered, because
 * hiding them hides exactly the pattern the void register exists to reveal.
 *
 * The API lists no individual receipts — GET /payments/:id fetches one, and
 * /reports/cashier-daily aggregates by operator, day and method rather than
 * enumerating slips. So this shows what *this browser* posted and says so,
 * rather than presenting a partial list as though it were the day's takings.
 */
function ShiftReceipts({ justPosted }: { justPosted: RecordPaymentResponse[] }) {
  const posted: PaymentView[] = justPosted.map((p) => p.payment);

  return (
    <Panel
      title="ما قبضتُه في هذه الجلسة"
      aside={
        <Link className="label" to="/reports/cashier-daily">
          كشف اليوم ←
        </Link>
      }
    >
      {posted.length === 0 ? (
        <EmptyState kind="not-yet" title="لم تقبض شيئاً بعد" />
      ) : (
        posted.map((payment) => (
          <Row key={payment.id}>
            <span className="grow">
              <Link className="ltr num" to={`/payments/${payment.id}`}>
                {payment.receipt_no ?? "—"}
              </Link>
            </span>
            <Money
              value={payment.amount}
              tone="plain"
              {...(payment.status === "voided"
                ? { title: `ملغى: ${payment.void_reason ?? ""}` }
                : {})}
            />
            {payment.status === "voided" && <Chip tone="void">ملغى</Chip>}
          </Row>
        ))
      )}
      <p className="note" style={{ marginTop: 8 }}>
        هذه وصولات هذه الجلسة على هذا المتصفح فقط. المجاميع المعتمدة في{" "}
        <Link to="/reports/cashier-daily">كشف اليوم</Link>.
      </p>
    </Panel>
  );
}

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return debounced;
}
