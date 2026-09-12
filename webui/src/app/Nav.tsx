import { useCallback, useEffect, useState } from "react";
import { NavLink, Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { RefundView, SettlementExceptionView, VoidRequestView } from "@/api/types";
import logoMark from "@/assets/logo-mark.svg";
import { Icon } from "@/components/Icon";
import { PLANES, type NavItem } from "./navigation";
import { ROLE_LABELS, useSession } from "./session";

/**
 * The rail: brand at the head, the planes in the middle, the operator at the
 * foot.
 *
 * An item the operator lacks the capability for is not rendered at all — the
 * one place §03 principle 05 does not apply, and deliberately: the principle
 * is about controls on an object you can see, while a whole section of
 * navigation the operator has no business in is scope, not a refusal to
 * explain. §04 puts it as "the menu is built from permissions, not hidden
 * with CSS".
 */
export function Nav() {
  const { user, can, signOut } = useSession();
  const counters = useCounters();
  const [rail, setRail] = useState(() => document.documentElement.dataset["nav"] === "rail");

  const initial = (user?.full_name ?? "؟").trim().charAt(0);

  // The collapse is a persisted preference, like density: an operator who
  // works the rail wants it back tomorrow. Ctrl+B toggles it by physical key
  // (event.code), so it works identically on an Arabic keyboard layout.
  const toggle = useCallback(() => {
    setRail((current) => {
      const next = !current;
      if (next) {
        document.documentElement.dataset["nav"] = "rail";
        localStorage.setItem("flowed.nav", "rail");
      } else {
        delete document.documentElement.dataset["nav"];
        localStorage.setItem("flowed.nav", "full");
      }
      return next;
    });
  }, []);

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if ((event.ctrlKey || event.metaKey) && event.code === "KeyB") {
        event.preventDefault();
        toggle();
      }
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [toggle]);

  return (
    <nav className="nav" aria-label="التنقّل الرئيسي">
      <button
        type="button"
        className="nav__collapse"
        onClick={toggle}
        title={rail ? "توسيع القائمة (Ctrl+B)" : "طيّ القائمة (Ctrl+B)"}
        aria-expanded={!rail}
      >
        <Icon name="back" size={13} />
      </button>

      <Link
        to="/"
        className="nav__brand"
        style={{ textDecoration: "none" }}
        title={rail ? "Flowed — الرئيسية" : undefined}
      >
        <img className="nav__brand-mark" src={logoMark} alt="" />
        <span className="nav__brand-text">
          <span className="nav__brand-name">Flowed</span>
          <span className="nav__brand-tag">نظام أجور وأقساط الطلبة</span>
        </span>
      </Link>

      <div className="nav__scroll">
        {PLANES.map((plane) => {
          const items = plane.items.filter((item) => !item.capability || can(item.capability));
          if (items.length === 0) return null;
          return (
            <div className="nav__plane" key={plane.title}>
              <h6 className="nav__title">
                {plane.title}
                <span className="nav__caption">{plane.caption}</span>
              </h6>
              <ul className="nav__list">
                {items.map((item) => (
                  <li key={item.to}>
                    <NavLink
                      to={item.to}
                      className={({ isActive }) => `nav__link${isActive ? " is-active" : ""}`}
                      title={rail ? item.label : (item.unavailable ?? undefined)}
                    >
                      <Icon name={item.icon} />
                      <span className="grow">{item.label}</span>
                      <Counter item={item} counters={counters} />
                      {item.unavailable && (
                        <span className="nav__planned" title={item.unavailable}>
                          —
                        </span>
                      )}
                    </NavLink>
                  </li>
                ))}
              </ul>
            </div>
          );
        })}
      </div>

      <div className="nav__user">
        <span className="nav__avatar" aria-hidden="true">
          {initial}
        </span>
        <span className="grow nav__user-text" style={{ minWidth: 0 }}>
          <Link to="/me/sessions" className="nav__user-name" style={{ display: "block", textDecoration: "none", color: "inherit" }} title="جلساتي">
            {user?.full_name}
          </Link>
          <span className="nav__user-role">
            {user?.roles.map((role) => ROLE_LABELS[role] ?? role).join(" · ")}
          </span>
        </span>
        <button type="button" className="nav__signout" title="خروج" onClick={() => void signOut()}>
          <Icon name="logout" size={15} />
        </button>
      </div>
    </nav>
  );
}

function Counter({
  item,
  counters,
}: {
  item: NavItem;
  counters: Record<string, number | undefined>;
}) {
  if (!item.counter) return null;
  const value = counters[item.counter];
  if (!value) return null;
  return <span className="nav__count">{value}</span>;
}

/**
 * Live counters on the work plane.
 *
 * These are why the inboxes are inboxes rather than tabs on a document page:
 * the number is what makes someone open one today rather than next week. The
 * endpoints return bare arrays, so the count is the length.
 */
function useCounters(): Record<string, number | undefined> {
  const { can } = useSession();

  const voids = useQuery({
    queryKey: ["voids", "pending"],
    queryFn: () => api.get<VoidRequestView[]>("/voids/pending"),
    enabled: can("payment.void.execute"),
    refetchInterval: 60_000,
  });

  const refunds = useQuery({
    queryKey: ["refunds", "pending"],
    queryFn: () => api.get<RefundView[]>("/refunds/pending"),
    enabled: can("refund.approve"),
    refetchInterval: 60_000,
  });

  const exceptions = useQuery({
    queryKey: ["settlements", "exceptions"],
    queryFn: () => api.get<SettlementExceptionView[]>("/settlements/exceptions"),
    enabled: can("settlement.write"),
    refetchInterval: 120_000,
  });

  return {
    voids: voids.data?.length,
    refunds: refunds.data?.length,
    settlement_exceptions: exceptions.data?.length,
  };
}
