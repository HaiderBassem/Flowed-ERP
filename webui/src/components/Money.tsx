import { useId, useMemo, useState } from "react";

import {
  type Amount,
  amount as toAmount,
  format,
  formatCount,
  formatWhileTyping,
  parseInput,
  spellArabic,
} from "@/lib/money";
import type { RawAmount } from "@/api/types";

/**
 * Money on screen.
 *
 * Every figure carries its sign explicitly and is set in tabular figures. §06
 * gives the reasons for both: colour alone disappears under colour blindness
 * and on the monochrome printer these end up on, and a column of amounts that
 * does not align is a column nobody audits by eye.
 */
export function Money({
  value,
  size = "normal",
  tone = "auto",
  sign = "auto",
  title,
}: {
  /** Raw wire value, an Amount, or null for "not computed". */
  value: RawAmount | Amount | null | undefined;
  size?: "normal" | "big" | "huge";
  /** `auto` colours negatives; `plain` never colours; `positive` marks a credit. */
  tone?: "auto" | "plain" | "positive";
  sign?: "auto" | "always" | "never";
  title?: string;
}) {
  // Zero is not blank (§06). null means the figure was not computed and shows
  // a dash with its reason; 0 means the system computed zero and says so.
  if (value === null || value === undefined) {
    return (
      <span className="money money--absent" title={title ?? "غير محسوب"}>
        —
      </span>
    );
  }

  const a = toAmount(value as bigint | number);
  const classes = ["money"];
  if (size === "big") classes.push("money--big");
  if (size === "huge") classes.push("money--huge");
  if (tone !== "plain") {
    if (a < 0n) classes.push("money--neg");
    else if (tone === "positive") classes.push("money--pos");
  }

  return (
    <span className={classes.join(" ")} {...(title ? { title } : {})}>
      {format(a, { sign })}
    </span>
  );
}

/** A count. Never styled as money — §11 keeps the two visually apart. */
export function Count({ value }: { value: number | null | undefined }) {
  return <span className="num">{formatCount(value)}</span>;
}

/**
 * MoneyField — §07's component, and the rules it carries are all from §06.
 *
 * Integers only, and the decimal point is refused *as it is typed* rather than
 * on submit, with the reason said out loud. Thousands separators appear live,
 * because seven adjacent digits on a window screen is a real source of error.
 * Arabic-Indic digits are accepted and converted on the spot — the operator's
 * keyboard may well be Arabic and refusing it is stubbornness with no benefit.
 * Above a million the written form appears beneath the field, since that is
 * what will be printed and what makes the figure hard to alter.
 */
export function MoneyField({
  value,
  onChange,
  label,
  max,
  autoFocus,
  disabled,
  inputRef,
  onEnter,
  id,
}: {
  value: string;
  onChange: (next: string) => void;
  label: string;
  /** A ceiling worth warning about — the remaining balance, usually. */
  max?: Amount | undefined;
  autoFocus?: boolean;
  disabled?: boolean;
  inputRef?: React.Ref<HTMLInputElement>;
  onEnter?: () => void;
  id?: string;
}) {
  const generatedId = useId();
  const fieldId = id ?? generatedId;
  const [touched, setTouched] = useState(false);

  const parsed = useMemo(() => parseInput(value), [value]);
  const spelled = useMemo(
    () => (parsed.ok && parsed.value > 0n ? spellArabic(parsed.value) : null),
    [parsed],
  );

  const overMax = parsed.ok && max !== undefined && parsed.value > max;
  const showError = touched && !parsed.ok && parsed.reason !== "empty";

  return (
    <div className="field">
      <label className="field__label" htmlFor={fieldId}>
        {label}
      </label>
      <input
        id={fieldId}
        ref={inputRef}
        className={`input input--money${showError || overMax ? " input--invalid" : ""}`}
        inputMode="numeric"
        autoComplete="off"
        dir="ltr"
        disabled={disabled ?? false}
        autoFocus={autoFocus ?? false}
        value={value}
        aria-describedby={`${fieldId}-hint`}
        aria-invalid={showError || overMax}
        onBlur={() => setTouched(true)}
        onKeyDown={(e) => {
          if (e.key === "Enter" && onEnter) {
            e.preventDefault();
            onEnter();
          }
        }}
        onChange={(e) => {
          const raw = e.target.value;
          // Formatted on every keystroke rather than on blur: the separator is
          // there to be read while typing, which is when the misreading
          // happens.
          onChange(formatWhileTyping(raw));
          if (!touched) setTouched(true);
        }}
      />

      <div id={`${fieldId}-hint`}>
        {showError && (
          <p className="field__error">
            {parsed.reason === "fraction"
              ? "الدينار لا يتجزأ في هذا النظام — أدخل عدداً صحيحاً"
              : parsed.reason === "negative"
                ? "المبلغ لا يكون سالباً — التصحيح يكون بإلغاء أو استرجاع أو قيد تسوية"
                : "أدخل رقماً"}
          </p>
        )}
        {overMax && max !== undefined && (
          <p className="field__error">
            المبلغ يتجاوز المتبقي ({format(max)}) — الفائض يتحوّل إلى رصيد دائن
          </p>
        )}
        {/* The written amount is what makes a receipt hard to alter, so it is
            shown before committing rather than discovered on the slip. */}
        {spelled && parsed.ok && parsed.value >= 1_000_000n && (
          <p className="field__hint">{spelled}</p>
        )}
      </div>
    </div>
  );
}
