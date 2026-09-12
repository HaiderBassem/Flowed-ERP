/**
 * Money in the browser.
 *
 * Amount is whole Iraqi dinars carried as a bigint, for the reason §13 gives:
 * a JS number loses integer precision above 2^53, and this interface displays
 * figures that are summed across a college. Nothing here computes a total —
 * §15 forbids it, and the reason is that a figure computed in the browser is a
 * second figure that can disagree with the server's. Everything below either
 * parses what the server sent, parses what the operator typed, or renders one
 * of the two.
 */

const BRAND: unique symbol = Symbol.for("flowed.amount");

export type Amount = bigint & { readonly [BRAND]?: "IQD" };

/** The largest integer a JSON number survives intact. */
const MAX_SAFE = BigInt(Number.MAX_SAFE_INTEGER);

/**
 * amount coerces a value that came from the API, or from a form, into Amount.
 *
 * A JSON number above 2^53 has already lost precision by the time it reaches
 * here — JSON.parse rounded it — so this cannot recover it, only refuse to
 * pass it off as exact. The API client keeps money fields as strings where it
 * can precisely so that this path stays theoretical.
 */
export function amount(value: bigint | number | string | null | undefined): Amount {
  if (value === null || value === undefined || value === "") return 0n as Amount;
  if (typeof value === "bigint") return value as Amount;
  if (typeof value === "string") {
    const trimmed = value.trim();
    if (!/^-?\d+$/.test(trimmed)) return 0n as Amount;
    return BigInt(trimmed) as Amount;
  }
  if (!Number.isFinite(value)) return 0n as Amount;
  if (!Number.isInteger(value)) {
    // The dinar does not subdivide in this system. A fraction here means a
    // contract mismatch, not a rounding decision to make quietly.
    throw new Error(`money: non-integer amount from API: ${value}`);
  }
  const big = BigInt(value);
  if (big > MAX_SAFE || big < -MAX_SAFE) {
    console.warn("money: amount exceeded safe integer range in transport", value);
  }
  return big as Amount;
}

/** Sum, for display groupings the server already agrees with (subtotals of rows it sent). */
export function sum(values: readonly Amount[]): Amount {
  let total = 0n;
  for (const v of values) total += v;
  return total as Amount;
}

export const isZero = (a: Amount): boolean => a === 0n;
export const isNegative = (a: Amount): boolean => a < 0n;
export const abs = (a: Amount): Amount => (a < 0n ? -a : a) as Amount;

/* ------------------------------------------------------------------ format */

/**
 * format renders an amount in Western digits with thousands separators.
 *
 * Western digits on screen and on paper alike, per the system's own rule: ٠
 * and 0 are confusable on a bad print, and a figure that can be misread is a
 * figure that can be disputed.
 *
 * The sign is written, not merely coloured. Colour alone drops out under
 * colour blindness and on the monochrome printer these documents end up on.
 */
export function format(a: Amount, options: { sign?: "auto" | "always" | "never" } = {}): string {
  const mode = options.sign ?? "auto";
  const negative = a < 0n;
  const digits = groupDigits((negative ? -a : a).toString());
  if (mode === "never") return digits;
  if (negative) return `−${digits}`; // U+2212 minus, not a hyphen
  return mode === "always" ? `+${digits}` : digits;
}

function groupDigits(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ",");
}

/**
 * Zero is not blank (§06).
 *
 * `0` means the system computed zero. Blank means it did not compute at all,
 * and shows an em dash with a title rather than a figure. Conflating the two
 * in a debt report is an expensive mistake, so the difference is a type here
 * and not a convention.
 */
export function formatOrDash(a: Amount | null | undefined): string {
  if (a === null || a === undefined) return "—";
  return format(a);
}

/** Plain grouped integer for counts — never money, so never signed or coloured. */
export function formatCount(n: number | bigint | null | undefined): string {
  if (n === null || n === undefined) return "—";
  return groupDigits(n.toString());
}

/* ------------------------------------------------------------------- input */

const ARABIC_INDIC = "٠١٢٣٤٥٦٧٨٩";
const EASTERN_ARABIC_INDIC = "۰۱۲۳۴۵۶۷۸۹";

/**
 * westernize converts Arabic-Indic and Persian digits to Western ones.
 *
 * The operator's keyboard may well be Arabic. Refusing those digits is
 * stubbornness with no benefit — the system's rule is about what is *shown*,
 * not about what may be typed.
 */
export function westernize(input: string): string {
  let out = "";
  for (const ch of input) {
    const arabic = ARABIC_INDIC.indexOf(ch);
    if (arabic >= 0) {
      out += String(arabic);
      continue;
    }
    const persian = EASTERN_ARABIC_INDIC.indexOf(ch);
    if (persian >= 0) {
      out += String(persian);
      continue;
    }
    out += ch;
  }
  return out;
}

export type MoneyInput =
  | { ok: true; value: Amount; text: string }
  | { ok: false; reason: "empty" | "fraction" | "invalid" | "negative"; text: string };

/**
 * parseInput reads what the operator typed into an amount.
 *
 * A decimal point is refused as it is typed rather than on submit, and the
 * refusal says why: the dinar does not subdivide in this system. Discovering
 * that after a round trip, with a student waiting, is the sort of delay that
 * makes a window keep a paper ledger on the side.
 */
export function parseInput(raw: string): MoneyInput {
  const text = westernize(raw).replace(/[\s,٬،]/g, "");
  if (text === "") return { ok: false, reason: "empty", text: "" };
  if (/[.٫]/.test(text)) return { ok: false, reason: "fraction", text };
  if (/^-/.test(text)) return { ok: false, reason: "negative", text };
  if (!/^\d+$/.test(text)) return { ok: false, reason: "invalid", text };
  return { ok: true, value: BigInt(text) as Amount, text };
}

/** Live thousands separators while typing: seven adjacent digits on a desk screen misread. */
export function formatWhileTyping(raw: string): string {
  const parsed = parseInput(raw);
  if (!parsed.ok) return westernize(raw);
  return groupDigits(parsed.value.toString());
}

/* ------------------------------------------------------------------ tafqit */

/**
 * Arabic number spelling — التفقيط.
 *
 * A faithful port of internal/domain/money/arabic.go, structure and comments
 * kept alongside so the two can be read against each other. It is duplicated
 * rather than fetched because §06 puts the written amount on the confirmation
 * sheet, before anything has been posted and so before the server has a
 * receipt to render — and because the written amount is what makes the figure
 * hard to alter, which only helps if the operator sees it before committing.
 *
 * The authority remains the server: the printed receipt carries the Go
 * implementation's output. money.spell.test.ts pins this port against vectors
 * generated from that implementation so the two cannot drift apart unnoticed.
 */

const ONES = ["", "واحد", "اثنان", "ثلاثة", "أربعة", "خمسة", "ستة", "سبعة", "ثمانية", "تسعة"];

// Ten through nineteen are irregular enough to list rather than compose.
const TEENS = [
  "عشرة", "أحد عشر", "اثنا عشر", "ثلاثة عشر", "أربعة عشر",
  "خمسة عشر", "ستة عشر", "سبعة عشر", "ثمانية عشر", "تسعة عشر",
];

const TENS = ["", "", "عشرون", "ثلاثون", "أربعون", "خمسون", "ستون", "سبعون", "ثمانون", "تسعون"];

// Hundreds are single words, and several are not the concatenation a reader
// might expect: three hundred is ثلاثمائة, not ثلاثة مائة.
const HUNDREDS = [
  "", "مائة", "مائتان", "ثلاثمائة", "أربعمائة", "خمسمائة",
  "ستمائة", "سبعمائة", "ثمانمائة", "تسعمائة",
];

interface Scale {
  singular: string;
  dual: string;
  plural: string;
  accusative: string;
}

const SCALES: readonly (Scale | null)[] = [
  null, // units: no scale word
  { singular: "ألف", dual: "ألفان", plural: "آلاف", accusative: "ألفاً" },
  { singular: "مليون", dual: "مليونان", plural: "ملايين", accusative: "مليوناً" },
  { singular: "مليار", dual: "ملياران", plural: "مليارات", accusative: "ملياراً" },
  { singular: "ترليون", dual: "ترليونان", plural: "ترليونات", accusative: "ترليوناً" },
];

// A word governing the noun that follows it (المضاف) sheds its ending. A dual
// loses its final nūn — مائتان alone, مائتا ألف in construct — and a scale word
// in the accusative loses its tanween: عشرون ألفاً alone, عشرون ألف دينار when
// it governs the currency.
const CONSTRUCT_FORMS: Record<string, string> = {
  // Duals.
  "مائتان": "مائتا",
  "ألفان": "ألفا",
  "مليونان": "مليونا",
  "ملياران": "مليارا",
  "ترليونان": "ترليونا",
  "اثنان": "اثنا",
  // Scale words in the accusative.
  "ألفاً": "ألف",
  "مليوناً": "مليون",
  "ملياراً": "مليار",
  "ترليوناً": "ترليون",
};

/** asConstruct puts a trailing governing word into its construct form. */
function asConstruct(phrase: string): string {
  if (phrase === "") return phrase;
  const cut = phrase.lastIndexOf(" ");
  const head = cut >= 0 ? phrase.slice(0, cut + 1) : "";
  const last = cut >= 0 ? phrase.slice(cut + 1) : phrase;
  const construct = CONSTRUCT_FORMS[last];
  return construct === undefined ? phrase : head + construct;
}

/**
 * asConstructDualOnly applies the dual rule but not the accusative one.
 *
 * Inside a number a count like عشرون legitimately takes the accusative before
 * its scale word. Only the scale word at the very end, which then governs the
 * currency noun, sheds its tanween.
 */
function asConstructDualOnly(phrase: string): string {
  const cut = phrase.lastIndexOf(" ");
  const last = cut >= 0 ? phrase.slice(cut + 1) : phrase;
  if (last.endsWith("اً")) return phrase;
  return asConstruct(phrase);
}

type CountedForm = "singular" | "dual" | "plural" | "accusative";

/**
 * formFor picks the form of the counted noun (التمييز).
 *
 * The rule turns on the LAST TWO DIGITS, which is the part that is easy to get
 * wrong. Eleven through ninety-nine take the singular accusative — أحد عشر ألفاً
 * — but a round hundred goes back to the plain singular: five hundred thousand
 * is خمسمائة ألف, never خمسمائة ألفاً. An implementation that simply tests
 * "eleven or more" produces the second, and an accountant reads it as a mistake
 * on the first pass.
 */
function formFor(count: bigint): CountedForm {
  if (count === 1n) return "singular";
  if (count === 2n) return "dual";
  const lastTwo = count % 100n;
  if (lastTwo === 0n) return "singular";
  if (lastTwo === 1n) return "singular";
  if (lastTwo === 2n) return "dual";
  if (lastTwo <= 10n) return "plural";
  return "accusative";
}

function spellUnder1000(n: bigint): string {
  if (n === 0n) return "";
  const parts: string[] = [];

  const hundreds = Number(n / 100n);
  if (hundreds > 0) parts.push(HUNDREDS[hundreds]!);

  const remainder = Number(n % 100n);
  if (remainder === 0) {
    // Nothing below the hundreds.
  } else if (remainder < 10) {
    parts.push(ONES[remainder]!);
  } else if (remainder < 20) {
    parts.push(TEENS[remainder - 10]!);
  } else {
    const ones = remainder % 10;
    const tens = Math.floor(remainder / 10);
    if (ones === 0) parts.push(TENS[tens]!);
    // Arabic states the unit before the ten: twenty-one is "one and twenty".
    else parts.push(`${ONES[ones]!} و${TENS[tens]!}`);
  }

  return parts.join(" و");
}

function spellGroup(group: bigint, scaleIndex: number): string {
  if (scaleIndex === 0) return spellUnder1000(group);
  const s = SCALES[scaleIndex]!;

  // One and two do not speak their count: one thousand is ألف, not واحد ألف,
  // and two thousand is ألفان.
  if (group === 1n) return s.singular;
  if (group === 2n) return s.dual;

  // The count governs the scale word that follows it, so a trailing dual in
  // the count drops its nūn: 200,000 is مائتا ألف, not مائتان ألف.
  const spelled = asConstructDualOnly(spellUnder1000(group));
  switch (formFor(group)) {
    case "singular":
      return `${spelled} ${s.singular}`;
    case "dual":
      return `${spelled} ${s.dual}`;
    case "plural":
      return `${spelled} ${s.plural}`;
    default:
      return `${spelled} ${s.accusative}`;
  }
}

function spellNumber(n: bigint): string {
  if (n === 0n) return "صفر";

  // Split into groups of three, least significant first, so each can be paired
  // with its scale word.
  const groups: bigint[] = [];
  let rest = n;
  while (rest > 0n) {
    groups.push(rest % 1000n);
    rest /= 1000n;
  }
  if (groups.length > SCALES.length) {
    // Beyond a trillion the scale words run out. No tuition reaches here, and
    // silently producing a wrong word on a financial document would be worse
    // than admitting the gap.
    return "مبلغ يتجاوز الحد القابل للكتابة";
  }

  const parts: string[] = [];
  for (let i = groups.length - 1; i >= 0; i--) {
    const group = groups[i]!;
    if (group === 0n) continue;
    parts.push(spellGroup(group, i));
  }
  return parts.join(" و");
}

/**
 * arabicDinars picks the form of "dinar" that agrees with the count.
 *
 * The agreement follows the final group of three digits, because that is what
 * the noun actually counts. A round thousand or million ends in zeroes, so the
 * noun takes the plain singular: one and a half million dinars is
 * مليون وخمسمائة ألف دينار عراقي.
 */
function arabicDinars(n: bigint): string {
  const group = n % 1000n;
  if (group === 0n) return "دينار عراقي";
  switch (formFor(group)) {
    case "singular":
      return "دينار عراقي";
    case "dual":
      return "ديناران عراقيان";
    case "plural":
      return "دنانير عراقية";
    default:
      return "ديناراً عراقياً";
  }
}

/**
 * spellArabic writes an amount of dinars in Arabic words, as an Iraqi receipt
 * carries it.
 *
 * The closing لا غير ("and no more") marks the end of the written amount so
 * nothing can be appended to it, and is joined with a non-breaking space:
 * split across two lines on a narrow thermal roll it stops reading as one
 * phrase, which is the whole point of it.
 */
export function spellArabic(a: Amount): string {
  if (a < 0n) return `سالب ${spellArabic(-a as Amount)}`;
  if (a === 0n) return "صفر دينار عراقي";

  // One and two are not counted out loud. The singular and the dual of the
  // noun already carry the number — دينار عراقي is one dinar and ديناران
  // عراقيان is two — so saying واحد or اثنان in front of them is the
  // redundancy of a translation rather than Arabic.
  if (a === 1n || a === 2n) return `${arabicDinars(a)} لا غير`;

  const spelled = asConstruct(spellNumber(a));
  return `${spelled} ${arabicDinars(a)} لا غير`;
}

/** The number in words without currency or closing marker. */
export function spellArabicPlain(a: Amount): string {
  if (a < 0n) return `سالب ${spellArabicPlain(-a as Amount)}`;
  if (a === 0n) return "صفر";
  return spellNumber(a);
}
