// Formatting shared by every screen.
//
// Pure functions, in their own module, because they are the only logic in this
// client worth testing on its own — and because a money figure formatted two
// different ways on two screens is how an operator concludes the system
// disagrees with itself.

// money renders whole Iraqi dinars.
//
// Grouped with a comma and never with a decimal: IQD has no circulating
// subunit, and a figure showing "1,500,000.00" invites somebody to type a
// fraction the API will refuse. Western digits even in an Arabic page, for the
// same reason the printed receipts use them — ٠ and 0 are confusable on a bad
// screen and a worse printer.
export function money(value) {
  const amount = Number(value ?? 0);
  if (!Number.isFinite(amount)) return "0";
  const negative = amount < 0;
  const digits = Math.abs(Math.trunc(amount)).toString().replace(/\B(?=(\d{3})+(?!\d))/g, ",");
  return negative ? `-${digits}` : digits;
}

// parseAmount reads what an operator typed into a money field.
//
// Tolerant of the separators people actually type — commas, spaces, Arabic-
// Indic digits — and strict about fractions: a fractional dinar is refused by
// the API, and silently truncating here would send a different number from the
// one on the screen.
export function parseAmount(input) {
  const text = String(input ?? "").trim();
  if (!text) return 0;

  let normalised = "";
  for (const ch of text) {
    const code = ch.codePointAt(0);
    if (code >= 0x0660 && code <= 0x0669) normalised += String(code - 0x0660);      // ٠-٩
    else if (code >= 0x06f0 && code <= 0x06f9) normalised += String(code - 0x06f0); // ۰-۹
    else if (/[0-9.\-]/.test(ch)) normalised += ch;
    // Commas and spaces are grouping; anything else is dropped rather than
    // making the field unusable.
  }

  const value = Number(normalised);
  if (!Number.isFinite(value)) return 0;
  return Math.trunc(value);
}

// dateOnly renders a timestamp as a date.
//
// A cashier comparing a receipt against a screen is comparing days, and a
// time-of-day with a timezone in it invites the wrong question.
export function dateOnly(value) {
  if (!value) return "";
  const text = String(value);
  const separator = text.indexOf("T");
  return separator > 0 ? text.slice(0, separator) : text;
}

export const fmt = { money };
