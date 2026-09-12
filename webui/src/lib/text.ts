/**
 * Arabic text on screen.
 *
 * The server folds names on write (generated columns) and folds the search
 * term on read, so a clerk typing فاطمه finds فاطمة. §06 asks the interface to
 * *show* that it happened — otherwise the match looks like a bug and the clerk
 * concludes the system fetched a different person.
 *
 * The fold below mirrors normalize_arabic() for display only. Nothing here is
 * ever sent as a query term: the server folds what it receives, and a client
 * that pre-folds would hide a divergence between the two implementations
 * instead of surfacing it.
 */

/** Tashkeel (harakat) and the superscript alef. */
const TASHKEEL = /[ً-ْٰـ]/g;

/**
 * fold applies the same orthographic normalisation the database does, for
 * showing the operator what their term became.
 */
export function fold(input: string): string {
  return input
    .replace(TASHKEEL, "") // tashkeel and tatweel
    .replace(/[أإآٱ]/g, "ا") // hamza carriers on alef
    .replace(/ة/g, "ه") // ta marbuta
    .replace(/ى/g, "ي") // alef maqsura
    .replace(/ؤ/g, "و")
    .replace(/ئ/g, "ي")
    .replace(/\s+/g, " ")
    .trim();
}

/** True when two names differ only in orthography — the case worth explaining. */
export function foldsTogether(a: string, b: string): boolean {
  return a !== b && fold(a) === fold(b);
}

/* ------------------------------------------------------------------ phone */

/**
 * Phone numbers are entered in any shape and shown in one.
 *
 * Search by the last four digits is supported because that is what people
 * actually remember, and it is why the display keeps those four legible rather
 * than running eleven digits together.
 */
export function normalisePhone(raw: string): string {
  const digits = raw.replace(/[^\d٠-٩۰-۹+]/g, "");
  let western = "";
  for (const ch of digits) {
    const ar = "٠١٢٣٤٥٦٧٨٩".indexOf(ch);
    const fa = "۰۱۲۳۴۵۶۷۸۹".indexOf(ch);
    western += ar >= 0 ? String(ar) : fa >= 0 ? String(fa) : ch;
  }
  // +964 7XX and 07XX are the same subscriber.
  if (western.startsWith("+964")) return `0${western.slice(4)}`;
  if (western.startsWith("964")) return `0${western.slice(3)}`;
  return western;
}

export function formatPhone(raw: string | null | undefined): string {
  if (!raw) return "—";
  const n = normalisePhone(raw);
  if (n.length !== 11) return n;
  return `${n.slice(0, 4)} ${n.slice(4, 7)} ${n.slice(7)}`;
}

export const lastFour = (raw: string | null | undefined): string =>
  raw ? normalisePhone(raw).slice(-4) : "";

/* ------------------------------------------------------------- identifiers */

/** Shorten a UUID for a dense grid, keeping it copyable in full via title. */
export function shortId(id: string): string {
  return id.length > 12 ? `${id.slice(0, 4)}…${id.slice(-4)}` : id;
}

/** Arabic pluralisation for the small counts that appear in labels. */
export function plural(n: number, one: string, two: string, few: string, many: string): string {
  if (n === 1) return one;
  if (n === 2) return two;
  if (n % 100 >= 3 && n % 100 <= 10) return few;
  return many;
}
