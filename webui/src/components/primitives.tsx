import type { ReactNode } from "react";

/** A titled panel — the unit every screen is assembled from. */
export function Panel({
  title,
  aside,
  children,
  flush,
  foot,
}: {
  title?: ReactNode;
  aside?: ReactNode;
  children: ReactNode;
  flush?: boolean;
  foot?: ReactNode;
}) {
  return (
    <section className="panel">
      {title !== undefined && (
        <header className="panel__head">
          <span className="grow">{title}</span>
          {aside}
        </header>
      )}
      <div className={`panel__body${flush ? " panel__body--flush" : ""}`}>{children}</div>
      {foot && <footer className="panel__foot">{foot}</footer>}
    </section>
  );
}

/** A labelled row inside a panel. */
export function Row({
  label,
  children,
  className,
}: {
  label?: ReactNode;
  children: ReactNode;
  className?: string;
}) {
  return (
    <div className={`row${className ? ` ${className}` : ""}`}>
      {label !== undefined && <span className="label grow">{label}</span>}
      {children}
    </div>
  );
}

export function Stat({
  value,
  label,
  tone,
}: {
  value: ReactNode;
  label: string;
  tone?: "live" | "void" | "pending" | "frozen" | "muted";
}) {
  const colour = tone
    ? {
        live: "var(--accent)",
        void: "var(--void)",
        pending: "var(--pending)",
        frozen: "var(--frozen)",
        muted: "var(--muted)",
      }[tone]
    : undefined;
  return (
    <div className="stat">
      <b style={colour ? { color: colour } : undefined}>{value}</b>
      <span>{label}</span>
    </div>
  );
}

/**
 * Empty states — three kinds, never one (§07).
 *
 * "No results for this search", "nothing here yet and the next step is X", and
 * "no permission and the reason is Y" are different facts. Rendering all three
 * as one blank panel hides a permission fault behind a screen that looks
 * perfectly healthy.
 *
 * `clean` is a fourth and separate case: reconciliation's healthy state is
 * empty, and §09 asks the interface to celebrate the emptiness rather than
 * present it as a failure to find anything.
 */
export function EmptyState({
  kind,
  title,
  detail,
  action,
}: {
  kind: "no-results" | "not-yet" | "forbidden" | "clean";
  title: string;
  detail?: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className={`empty empty--${kind}`}>
      <p className="empty__title">{title}</p>
      {detail && <div>{detail}</div>}
      {action && <div className="empty__action">{action}</div>}
    </div>
  );
}

export function Kbd({ children }: { children: ReactNode }) {
  return <kbd className="kbd">{children}</kbd>;
}

/**
 * A control the operator may not use, shown disabled and wearing its reason.
 *
 * §03 principle 05: hiding it teaches the operator the operation does not
 * exist, and they route around it by a worse means.
 */
export function Button({
  children,
  onClick,
  variant = "default",
  size,
  disabled,
  disabledReason,
  type = "button",
  title,
  busy,
}: {
  children: ReactNode;
  onClick?: () => void;
  variant?: "default" | "primary" | "danger" | "ghost";
  size?: "sm" | "lg";
  disabled?: boolean;
  /** Why this operator cannot do it. Shown on hover and to assistive tech. */
  disabledReason?: string | undefined;
  type?: "button" | "submit";
  title?: string;
  busy?: boolean;
}) {
  const isDisabled = Boolean(disabled) || Boolean(busy);
  const classes = ["btn"];
  if (variant !== "default") classes.push(`btn--${variant}`);
  if (size) classes.push(`btn--${size}`);

  return (
    <button
      type={type}
      className={classes.join(" ")}
      onClick={onClick ?? undefined}
      disabled={isDisabled}
      title={disabledReason ?? title}
      {...(disabledReason ? { "data-reason": disabledReason } : {})}
      aria-describedby={undefined}
    >
      {children}
    </button>
  );
}

export function Callout({
  children,
  variant,
}: {
  children: ReactNode;
  variant?: "warn" | "note";
}) {
  return <div className={`callout${variant ? ` callout--${variant}` : ""}`}>{children}</div>;
}

/** A skeleton the same height as the content it stands in for — §12 wants
 *  structure immediately and data after, never a white screen. */
export function Skeleton({ width = "100%", height = 16 }: { width?: string | number; height?: number }) {
  return <span className="skeleton" style={{ display: "block", width, height }} />;
}
