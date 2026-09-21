import { useId } from "react";

/**
 * An Iraqi mobile number, entered as +964 plus the 10-digit subscriber
 * number.
 *
 * The country code is fixed and shown separately rather than typed, so the
 * only thing the operator enters is the part that actually varies. The field
 * hard-limits input to ten digits and refuses anything that is not a real
 * Iraqi mobile prefix (7 followed by 3..9) — the same rule the server enforces
 * in `student.NormalizeIraqiPhone`, so a number this field accepts is never
 * refused a moment later by the API.
 */

const IRAQI_MOBILE_COMPLETE = /^7[3-9][0-9]{8}$/;

export function isCompleteIraqiMobile(digits: string): boolean {
  return IRAQI_MOBILE_COMPLETE.test(digits);
}

/** Strips a stored/typed phone value down to the bare 10-digit subscriber
 * number this input edits, for prefilling an edit form. */
export function subscriberDigitsOf(stored: string | null | undefined): string {
  if (!stored) return "";
  const digits = stored.replace(/\D/g, "");
  if (digits.startsWith("964")) return digits.slice(3);
  if (digits.startsWith("0")) return digits.slice(1);
  return digits;
}

/** Composes the value sent to the API from the edited subscriber digits. */
export function toApiPhone(digits: string): string | undefined {
  return digits ? `+964${digits}` : undefined;
}

export function IraqiPhoneInput({
  label,
  value,
  onChange,
  required,
  hint,
}: {
  label: string;
  /** Bare subscriber digits, e.g. "7701234567" — no leading 0, no country code. */
  value: string;
  onChange: (digits: string) => void;
  required?: boolean;
  hint?: string;
}) {
  const id = useId();
  const invalid = value.length > 0 && !isCompleteIraqiMobile(value);

  return (
    <label className="field" htmlFor={id}>
      <span className="field__label">{label}</span>
      <div className="phone-input">
        <span className="phone-input__prefix">+964</span>
        <input
          id={id}
          className={`input ltr${invalid ? " input--invalid" : ""}`}
          inputMode="numeric"
          autoComplete="tel-national"
          value={value}
          maxLength={10}
          placeholder="7XXXXXXXXX"
          required={required}
          onChange={(e) => onChange(e.target.value.replace(/\D/g, "").slice(0, 10))}
        />
      </div>
      {invalid && (
        <span className="field__error">
          رقم عراقي غير صالح — 10 أرقام تبدأ بـ 7 ثم رقم من 3 إلى 9
        </span>
      )}
      {!invalid && hint && <span className="field__hint">{hint}</span>}
    </label>
  );
}
