import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { Page, StudentView } from "@/api/types";
import { fold } from "@/lib/text";
import { PLANES } from "./navigation";
import { useSession, type Capability } from "./session";

/**
 * The command palette — §04.
 *
 * Searches three domains at once and keeps them visually distinct: objects (a
 * student number, a normalised Arabic name, a receipt number, the last four
 * digits of a phone), commands (by their domain names — "قبض دفعة", "طلب
 * إلغاء"), and destinations (screens and reports).
 *
 * A command the operator may not run appears **disabled with its reason**
 * rather than being filtered out, which is §03 principle 05 applied to search
 * results: an operator who never sees a command concludes it does not exist.
 */

interface Command {
  id: string;
  label: string;
  to: string;
  capability?: Capability;
}

const COMMANDS: Command[] = [
  { id: "record-payment", label: "قبض دفعة", to: "/desk", capability: "payment.record" },
  { id: "open-shift", label: "فتح وردية", to: "/desk/session/open", capability: "shift.open" },
  { id: "close-shift", label: "إغلاق الوردية ومصالحة الدرج", to: "/desk/sessions", capability: "shift.close" },
  { id: "void-inbox", label: "طلبات الإلغاء", to: "/inbox/voids", capability: "payment.void.execute" },
  { id: "refund-inbox", label: "الاسترجاعات", to: "/inbox/refunds", capability: "refund.approve" },
  { id: "register-student", label: "تسجيل طالب جديد", to: "/students/new", capability: "student.register" },
  { id: "enroll", label: "تسجيل في سنة", to: "/enrollments/new", capability: "enrollment.write" },
  { id: "bulk-accounts", label: "توليد حسابات جماعي", to: "/bulk/accounts", capability: "account.generate" },
  { id: "bulk-promote", label: "ترقية جماعية", to: "/bulk/promotions", capability: "enrollment.result" },
  { id: "import", label: "استيراد طلبة", to: "/imports", capability: "import.run" },
  { id: "fee-resolution", label: "مُجرِّب ترجيح سياسات الرسوم", to: "/config/fee-policies", capability: "config.write" },
  { id: "templates", label: "قوالب الأقساط", to: "/config/installment-templates", capability: "config.write" },
  { id: "discount-defs", label: "تعريفات الخصومات", to: "/config/discounts", capability: "config.write" },
  { id: "operators", label: "المستخدمون والجلسات", to: "/admin/operators", capability: "operators.administer" },
  { id: "backups", label: "النسخ الاحتياطي", to: "/admin/backups", capability: "backup.manage" },
  { id: "sponsors", label: "الكفلاء ومستحقاتهم", to: "/sponsors", capability: "settlement.read" },
  { id: "settlements", label: "التسويات البنكية", to: "/settlements", capability: "settlement.read" },
  { id: "settlement-exceptions", label: "استثناءات التسوية", to: "/settlements?tab=exceptions", capability: "settlement.write" },
  { id: "hosting", label: "اتفاقيات الاستضافة", to: "/hosting", capability: "hosting.write" },
  { id: "my-sessions", label: "جلساتي", to: "/me/sessions" },
  { id: "reconciliation", label: "المصالحة", to: "/oversight/reconciliation", capability: "oversight.read" },
  { id: "audit-chain", label: "تحقق سلسلة التدقيق", to: "/oversight/audit", capability: "oversight.read" },
];

export function CommandPalette({ open, onClose }: { open: boolean; onClose: () => void }) {
  const [term, setTerm] = useState("");
  const [cursor, setCursor] = useState(0);
  const navigate = useNavigate();
  const { can, reason } = useSession();
  const inputRef = useRef<HTMLInputElement>(null);

  useEffect(() => {
    if (open) {
      setTerm("");
      setCursor(0);
      window.setTimeout(() => inputRef.current?.focus(), 0);
    }
  }, [open]);

  // Objects come from the server, which folds the term the same way the stored
  // columns were folded. Debounced because this fires on every keystroke at a
  // window where the network is not fast.
  const debounced = useDebounced(term, 200);
  const students = useQuery({
    queryKey: ["students", "search", debounced],
    queryFn: () =>
      api.get<Page<StudentView>>("/students", { query: { q: debounced, limit: 6 } }),
    enabled: open && debounced.trim().length >= 2,
  });

  const destinations = useMemo(
    () =>
      PLANES.flatMap((plane) =>
        plane.items
          .filter((item) => !item.capability || can(item.capability))
          .map((item) => ({ label: item.label, to: item.to, plane: plane.title })),
      ).filter((d) => matches(d.label, term)),
    [term, can],
  );

  const commands = useMemo(() => COMMANDS.filter((c) => matches(c.label, term)), [term]);
  const found = students.data?.data ?? [];

  const rows = useMemo(
    () => [
      ...found.map((s) => ({ kind: "object" as const, key: s.id, student: s })),
      ...commands.map((c) => ({ kind: "command" as const, key: c.id, command: c })),
      ...destinations.map((d) => ({ kind: "destination" as const, key: d.to, destination: d })),
    ],
    [found, commands, destinations],
  );

  useEffect(() => {
    if (cursor >= rows.length) setCursor(Math.max(0, rows.length - 1));
  }, [rows.length, cursor]);

  if (!open) return null;

  const go = (index: number) => {
    const row = rows[index];
    if (!row) return;
    if (row.kind === "object") navigate(`/students/${row.student.id}`);
    else if (row.kind === "command") {
      if (row.command.capability && !can(row.command.capability)) return;
      navigate(row.command.to);
    } else navigate(row.destination.to);
    onClose();
  };

  return (
    <>
      <div className="overlay" onClick={onClose} />
      <div className="palette" role="dialog" aria-modal="true" aria-label="لوحة الأوامر">
        <input
          ref={inputRef}
          className="palette__input"
          placeholder="رقم جامعي، اسم، رقم وصل، آخر 4 أرقام هاتف، أو اسم أمر…"
          value={term}
          onChange={(e) => setTerm(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "ArrowDown") {
              e.preventDefault();
              setCursor((c) => Math.min(c + 1, rows.length - 1));
            } else if (e.key === "ArrowUp") {
              e.preventDefault();
              setCursor((c) => Math.max(c - 1, 0));
            } else if (e.key === "Enter") {
              e.preventDefault();
              go(cursor);
            } else if (e.key === "Escape") {
              onClose();
            }
          }}
        />

        {/* Showing the fold is the point: without it, searching فاطمه and
            getting فاطمة reads as the system having fetched someone else. */}
        {term.trim().length >= 2 && fold(term) !== term.trim() && (
          <p className="palette__fold">
            طُبِّع البحث إلى <span className="ltr">{fold(term)}</span> — النتائج تشمل الصيغتين
          </p>
        )}

        <div className="palette__results">
          {rows.length === 0 && (
            <p className="palette__empty">
              {term.trim().length < 2 ? "اكتب حرفين على الأقل" : "لا نتائج لهذا البحث"}
            </p>
          )}

          {rows.map((row, index) => {
            const active = index === cursor;
            if (row.kind === "object") {
              return (
                <button
                  key={row.key}
                  type="button"
                  className={`palette__row${active ? " is-active" : ""}`}
                  onMouseEnter={() => setCursor(index)}
                  onClick={() => go(index)}
                >
                  <span className="palette__kind">طالب</span>
                  <span className="grow">
                    <b>{row.student.full_name}</b>{" "}
                    {/* The mother's name is a permanent column, not a
                        secondary detail: two names identical after
                        normalisation are told apart by it. */}
                    <span className="label">— الأم: {row.student.mother_name}</span>
                  </span>
                  <span className="num">{row.student.student_no}</span>
                </button>
              );
            }
            if (row.kind === "command") {
              const blocked = row.command.capability ? reason(row.command.capability) : undefined;
              return (
                <button
                  key={row.key}
                  type="button"
                  className={`palette__row${active ? " is-active" : ""}`}
                  onMouseEnter={() => setCursor(index)}
                  onClick={() => go(index)}
                  disabled={Boolean(blocked)}
                  title={blocked}
                >
                  <span className="palette__kind palette__kind--command">أمر</span>
                  <span className="grow">{row.command.label}</span>
                  {blocked && <span className="label">{blocked}</span>}
                </button>
              );
            }
            return (
              <button
                key={row.key}
                type="button"
                className={`palette__row${active ? " is-active" : ""}`}
                onMouseEnter={() => setCursor(index)}
                onClick={() => go(index)}
              >
                <span className="palette__kind palette__kind--dest">وجهة</span>
                <span className="grow">{row.destination.label}</span>
                <span className="label">{row.destination.plane}</span>
              </button>
            );
          })}
        </div>
      </div>
    </>
  );
}

function matches(label: string, term: string): boolean {
  const t = fold(term).trim();
  if (t.length === 0) return true;
  return fold(label).includes(t);
}

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return debounced;
}
