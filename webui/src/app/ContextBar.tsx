import { useState } from "react";
import { Link } from "react-router-dom";

import { Chip, StateChip } from "@/components/Chip";
import { Kbd } from "@/components/primitives";
import { useSession } from "./session";
import { useWorkingContext } from "./working-context";

/**
 * The context bar — §04's "most important forty pixels in the system".
 *
 * Fixed at the top of every screen and not dismissible. It carries the three
 * facts that change whether an operation is legitimate — the active year and
 * its state, the organisational scope, the drawer session — and it changes
 * appearance when the state does. Identity and sign-out live on the sidebar;
 * this bar is reserved for what alters legitimacy.
 *
 * The second and third rows appear only when the condition exists, and they
 * appear **for every operator, not only the administrator who caused them**. A
 * settlement window is a temporary exception; hiding it from everyone else is
 * exactly how a controlled exception becomes a permanent hole.
 */
export function ContextBar() {
  const { is } = useSession();
  const { years, activeYear, setActiveYear } = useWorkingContext();

  return (
    <header className="ctx">
      <div className="ctx__row">
        <label className="ctx__pill ctx__pill--input">
          <span className="label">السنة</span>
          <select
            className="ctx__select"
            value={activeYear?.code ?? ""}
            onChange={(e) => setActiveYear(e.target.value)}
            aria-label="السنة الدراسية الفعّالة"
          >
            {years.length === 0 && <option value="">—</option>}
            {years.map((year) => (
              <option key={year.id} value={year.code}>
                {year.code}
              </option>
            ))}
          </select>
        </label>
        {activeYear && <StateChip entity="year" status={activeYear.status} />}

        <span className="ctx__sep" />
        <ScopeBadge />

        {is("cashier") && (
          <>
            <span className="ctx__sep" />
          </>
        )}

        <span className="grow" />

        <DensitySwitch />
        <ThemeSwitch />
        <span className="ctx__keys" title="لوحة الأوامر">
          <Kbd>Ctrl</Kbd>
          <Kbd>K</Kbd>
        </span>
      </div>

      <YearStateBanner />
    </header>
  );
}

/**
 * The density switch — §05 and §09 principle 09. Three modes, chosen by the
 * operator and remembered; applied on <html> so it lands before the next
 * paint.
 */
function DensitySwitch() {
  const [density, setDensity] = useState(
    () => document.documentElement.dataset["density"] ?? "default",
  );

  const apply = (next: string) => {
    setDensity(next);
    if (next === "default") delete document.documentElement.dataset["density"];
    else document.documentElement.dataset["density"] = next;
    localStorage.setItem("flowed.density", next);
  };

  return (
    <label className="ctx__pill ctx__pill--input" title="كثافة الصفوف">
      <span className="label">الكثافة</span>
      <select
        className="ctx__select"
        value={density}
        onChange={(e) => apply(e.target.value)}
        aria-label="كثافة الصفوف"
      >
        <option value="compact">مضغوط</option>
        <option value="default">افتراضي</option>
        <option value="comfortable">مريح</option>
      </select>
    </label>
  );
}

/**
 * Light, dark, or whatever the machine says. Dark is never the default (§15):
 * windows are worked in daylight.
 */
function ThemeSwitch() {
  const [theme, setTheme] = useState(() => document.documentElement.dataset["theme"] ?? "system");

  const apply = (next: string) => {
    setTheme(next);
    if (next === "system") {
      delete document.documentElement.dataset["theme"];
      localStorage.removeItem("flowed.theme");
    } else {
      document.documentElement.dataset["theme"] = next;
      localStorage.setItem("flowed.theme", next);
    }
  };

  return (
    <label className="ctx__pill ctx__pill--input" title="المظهر">
      <select
        className="ctx__select"
        value={theme}
        onChange={(e) => apply(e.target.value)}
        aria-label="المظهر"
      >
        <option value="system">حسب النظام</option>
        <option value="light">فاتح</option>
        <option value="dark">داكن</option>
      </select>
    </label>
  );
}

/**
 * §07's ScopeBadge.
 *
 * `scope_mode` from the server is "university" (the whole institution) or
 * "scoped" (named colleges). "university" is the unrestricted mode — reading
 * it as a restriction and labelling the badge "مقيّد — university" told every
 * full-access operator they were fenced in, which is precisely the confusion
 * a scope badge exists to prevent.
 */
function ScopeBadge() {
  const { user } = useSession();
  const scope = user?.scope_mode;
  const restricted = scope === "scoped";
  return (
    <span
      className="ctx__pill"
      title={
        restricted
          ? "نطاقك مقيّد بكليات محددة — كل قائمة تعرض ما داخله فقط، ولا فلترة صامتة"
          : "نطاقك التنظيمي: الجامعة كاملة"
      }
    >
      النطاق: {restricted ? "مقيّد بكليات" : "الجامعة كاملة"}
    </span>
  );
}

/**
 * The second and third rows.
 *
 * A financially closed year still collects: the payment posts against the open
 * year and allocates to the old year's installments. Saying that here, once,
 * saves the question being asked at the window with a student waiting.
 */
function YearStateBanner() {
  const { years, activeYear } = useWorkingContext();

  const closedWithDebt = years.filter((y) => y.status === "financially_closed");
  const settlementWindows = years.filter(
    (y) => y.status === "closed" && y.accepts_financial_posting,
  );

  return (
    <>
      {activeYear?.status === "financially_closed" && (
        <div className="banner banner--pending">
          <Chip tone="pending">سنة {activeYear.code} مغلقة مالياً</Chip>
          <span className="banner__note">
            القبض على أقساطها مسموح ويُرحَّل بوصل السنة الحالية · التعديل عليها مرفوض
          </span>
          <span className="grow" />
        </div>
      )}

      {activeYear?.status !== "financially_closed" && closedWithDebt.length > 0 && (
        <div className="banner banner--frozen">
          <Chip tone="frozen">
            {closedWithDebt.length === 1
              ? `سنة ${closedWithDebt[0]!.code} مغلقة مالياً`
              : `${closedWithDebt.length} سنوات مغلقة مالياً`}
          </Chip>
          <span className="banner__note">
            ديونها ما زالت تُحصَّل — القبض يُرحَّل بوصل السنة الحالية ويُخصَّص لأقساطها
          </span>
          <span className="grow" />
        </div>
      )}

      {/* A timed exception, announced to everyone until it closes. */}
      {settlementWindows.map((year) => (
        <div className="banner banner--void" key={year.id}>
          <Chip tone="void">نافذة تسوية مفتوحة على {year.code}</Chip>
          <span className="banner__note">
            سنة مغلقة أُعيد فتحها للتسوية — كل ما يُكتب عليها الآن استثناء موقوت
          </span>
          <span className="grow" />
          <Link className="btn btn--danger btn--sm" to={`/years/${year.id}`}>
            كونسول السنة
          </Link>
        </div>
      ))}
    </>
  );
}
