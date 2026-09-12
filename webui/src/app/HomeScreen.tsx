import { Link } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import type { RefundView, VoidRequestView } from "@/api/types";
import logoMark from "@/assets/logo-mark.svg";
import { Icon } from "@/components/Icon";
import { Kbd, Panel } from "@/components/primitives";
import { useWorkingContext } from "./working-context";
import { PLANES } from "./navigation";
import { ROLE_LABELS, useSession } from "./session";

/**
 * The landing screen.
 *
 * Deliberately **not** a dashboard (§15): nobody here starts their day with
 * charts. The cashier starts with a search, the finance manager with what is
 * waiting on their signature, the auditor with "is the system sound". So the
 * hero routes to that, the numbers shown are the ones that summon a person
 * (pending signatures, an unopened drawer), and the rest is a launcher.
 *
 * Voice per the identity: declarative, no exclamation marks, no celebration.
 */
export function HomeScreen() {
  const { user, can } = useSession();
  const { shift, activeYear } = useWorkingContext();

  const voids = useQuery({
    queryKey: ["voids", "pending"],
    queryFn: () => api.get<VoidRequestView[]>("/voids/pending"),
    enabled: can("payment.void.execute"),
  });
  const refunds = useQuery({
    queryKey: ["refunds", "pending"],
    queryFn: () => api.get<RefundView[]>("/refunds/pending"),
    enabled: can("refund.approve"),
  });

  const pendingSignatures = (voids.data?.length ?? 0) + (refunds.data?.length ?? 0);

  const start = can("payment.record")
    ? {
        to: shift?.status === "open" ? "/desk" : "/desk/session/open",
        label: shift?.status === "open" ? "شبّاك القبض" : "افتح وردية أولاً",
        why:
          shift?.status === "open"
            ? "الوردية مفتوحة. يومك يبدأ ببحث عن طالب."
            : "لا يمكن القبض قبل فتح وردية على شبّاكك.",
      }
    : can("payment.void.execute")
      ? {
          to: "/inbox/voids",
          label: "صندوق الوارد",
          why:
            pendingSignatures > 0
              ? `${pendingSignatures} وثيقة تنتظر توقيعك. الوثيقة المعلّقة تنتظر شخصاً، لا نظاماً.`
              : "لا شيء ينتظر توقيعك الآن.",
        }
      : can("oversight.read")
        ? {
            to: "/oversight/reconciliation",
            label: "المصالحة",
            why: "ابدأ بالسؤال الذي يسبق كل الأسئلة: هل النظام سليم.",
          }
        : { to: "/students", label: "الطلبة", why: "ابدأ من الكائن." };

  return (
    <main className="screen">
      {/* The hero: brand ground, one primary route, the working year. */}
      <section className="home-hero">
        <img className="home-hero__mark" src={logoMark} alt="" />
        <div className="grow">
          <h1 className="home-hero__title">أهلاً، {user?.full_name}</h1>
          <p className="home-hero__sub">
            {user?.roles.map((role) => ROLE_LABELS[role] ?? role).join(" · ")}
            {activeYear && (
              <>
                {" — "}السنة الفعّالة <b className="num">{activeYear.code}</b>
              </>
            )}
          </p>
          <p className="home-hero__why">{start.why}</p>
          <div className="cluster" style={{ marginTop: 12 }}>
            <Link className="btn btn--primary btn--lg" to={start.to}>
              {start.label}
            </Link>
            <span className="label">
              أو <Kbd>Ctrl</Kbd>+<Kbd>K</Kbd> للوصول إلى أي شيء بالاسم
            </span>
          </div>
        </div>
        {pendingSignatures > 0 && (
          <Link to="/inbox/voids" className="home-hero__badge" title="وثائق تنتظر توقيعك">
            <b className="num">{pendingSignatures}</b>
            <span>بانتظار توقيعك</span>
          </Link>
        )}
      </section>

      {/* The launcher: every plane the operator can act in, with its icons. */}
      <div className="cols cols--thirds" style={{ marginTop: 16 }}>
        {PLANES.map((plane) => {
          const items = plane.items.filter((item) => !item.capability || can(item.capability));
          if (items.length === 0) return null;
          return (
            <Panel
              key={plane.title}
              title={plane.title}
              aside={<span className="label">{plane.caption}</span>}
            >
              {items.map((item) => (
                <div className="row" key={item.to}>
                  <Icon name={item.icon} className="home-launch__icon" />
                  <Link className="grow" to={item.to} style={{ textDecoration: "none", color: "inherit" }}>
                    {item.label}
                  </Link>
                  {item.unavailable && (
                    <span className="label" title={item.unavailable}>
                      غير متاح
                    </span>
                  )}
                </div>
              ))}
            </Panel>
          );
        })}
      </div>
    </main>
  );
}
