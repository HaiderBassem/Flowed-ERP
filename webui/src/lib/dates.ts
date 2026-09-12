/**
 * Dates, as §06 requires them.
 *
 * The Gregorian calendar is the official one in every field and document here.
 * Hijri appears alongside where it helps and is never an input: two calendars
 * accepting input is two ways to record one due date.
 *
 * The week starts on Saturday and the weekend is Friday–Saturday. A date
 * picker laid out Sunday-first makes choosing an Iraqi due date quietly
 * error-prone.
 */

export const WEEK_START = 6; // Saturday, in JS getDay() terms
export const WEEKEND = [5, 6]; // Friday, Saturday

const AR_WEEKDAYS = ["الأحد", "الاثنين", "الثلاثاء", "الأربعاء", "الخميس", "الجمعة", "السبت"];

/** Day names in the order the calendar shows them: Saturday first. */
export const WEEKDAY_HEADINGS = [
  AR_WEEKDAYS[6]!,
  AR_WEEKDAYS[0]!,
  AR_WEEKDAYS[1]!,
  AR_WEEKDAYS[2]!,
  AR_WEEKDAYS[3]!,
  AR_WEEKDAYS[4]!,
  AR_WEEKDAYS[5]!,
];

/** Today at local midnight — the reference for anything overdue. */
export function today(): Date {
  const now = new Date();
  return new Date(now.getFullYear(), now.getMonth(), now.getDate());
}

export function parseDate(iso: string | null | undefined): Date | null {
  if (!iso) return null;
  const date = new Date(iso.length <= 10 ? `${iso}T00:00:00` : iso);
  return Number.isNaN(date.getTime()) ? null : date;
}

const pad = (n: number) => String(n).padStart(2, "0");

/** ISO date in Western digits — the form every column and document uses. */
export function formatDate(value: string | Date | null | undefined): string {
  const date = value instanceof Date ? value : parseDate(value ?? null);
  if (!date) return "—";
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}`;
}

/** Date and minute, for an event in a timeline or an audit row. */
export function formatDateTime(value: string | Date | null | undefined): string {
  const date = value instanceof Date ? value : parseDate(value ?? null);
  if (!date) return "—";
  return `${formatDate(date)} ${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

/** Hijri, shown beside the Gregorian date where it helps. Never an input. */
export function formatHijri(value: string | Date | null | undefined): string | null {
  const date = value instanceof Date ? value : parseDate(value ?? null);
  if (!date) return null;
  try {
    return new Intl.DateTimeFormat("ar-SA-u-ca-islamic-umalqura-nu-latn", {
      day: "numeric",
      month: "long",
      year: "numeric",
    }).format(date);
  } catch {
    // An old lab browser without the calendar data. The Gregorian date beside
    // it is the official one, so its absence costs nothing.
    return null;
  }
}

/**
 * daysOverdue derives lateness rather than reading it.
 *
 * Overdue is never stored — it is `due_date < today` with money outstanding —
 * so it is computed at the moment of display. An installment that fell due at
 * midnight must read as overdue in the morning without a reload, which is why
 * useToday() below re-renders the screen when the date turns over.
 */
export function daysOverdue(dueDate: string | null | undefined, reference = today()): number {
  const due = parseDate(dueDate);
  if (!due) return 0;
  const ms = reference.getTime() - due.getTime();
  if (ms <= 0) return 0;
  return Math.floor(ms / 86_400_000);
}

/** Elapsed time between two instants, as §07's approval card shows a gap. */
export function elapsed(from: string | Date, to: string | Date = new Date()): string {
  const a = from instanceof Date ? from : parseDate(from);
  const b = to instanceof Date ? to : parseDate(to);
  if (!a || !b) return "—";
  let seconds = Math.max(0, Math.floor((b.getTime() - a.getTime()) / 1000));
  const days = Math.floor(seconds / 86_400);
  seconds -= days * 86_400;
  const hours = Math.floor(seconds / 3_600);
  seconds -= hours * 3_600;
  const minutes = Math.floor(seconds / 60);
  if (days > 0) return `${days}ي ${hours}س`;
  if (hours > 0) return `${hours}س ${minutes}د`;
  return `${minutes}د`;
}

/** A running shift clock: HH:MM:SS since it opened. */
export function duration(fromIso: string, now: Date = new Date()): string {
  const from = parseDate(fromIso);
  if (!from) return "—";
  const total = Math.max(0, Math.floor((now.getTime() - from.getTime()) / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return `${pad(h)}:${pad(m)}:${pad(s)}`;
}

export const isWeekend = (date: Date): boolean => WEEKEND.includes(date.getDay());
