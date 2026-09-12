import { Link, useNavigate } from "react-router-dom";

import { Icon } from "./Icon";

export interface Crumb {
  label: string;
  /** Absent on the last crumb — the page the operator is standing on. */
  to?: string;
}

/**
 * The breadcrumb — the answer to "where am I, and how do I get back".
 *
 * Detail pages here nest three and four levels deep (student → enrollment →
 * account → receipt), and the raw route path that used to sit in this spot
 * answered neither question: a UUID in a URL locates a row, not a person.
 *
 * The back control uses history when there is any, so a cashier who arrived
 * from a filtered search returns to the same results, same scroll, same
 * query — navigation must not throw their context away. When the page was
 * opened cold (a pasted link), history is empty and the first crumb is the
 * honest destination instead.
 *
 * RTL note: the separator is a directional glyph chosen for reading order,
 * and the back chevron comes from the icon set so it mirrors with the layout
 * rather than pointing the wrong way as a hardcoded arrow would.
 */
export function Crumbs({ items }: { items: Crumb[] }) {
  const navigate = useNavigate();
  const parent = items.length > 1 ? items[items.length - 2] : undefined;

  return (
    <nav className="crumbs no-print" aria-label="مسار الصفحة">
      <button
        type="button"
        className="crumbs__back"
        title={parent?.to ? `رجوع إلى ${parent.label}` : "رجوع"}
        onClick={() => {
          // History first: it restores the exact previous state (search terms,
          // filters, scroll). The parent link is the fallback for a cold open.
          if (window.history.length > 1) navigate(-1);
          else if (parent?.to) navigate(parent.to);
          else navigate("/");
        }}
      >
        <Icon name="back" size={14} />
        <span>رجوع</span>
      </button>

      <ol className="crumbs__list">
        {items.map((item, index) => {
          const last = index === items.length - 1;
          return (
            <li key={`${item.label}-${index}`} className="crumbs__item">
              {item.to && !last ? (
                <Link to={item.to} className="crumbs__link">
                  {item.label}
                </Link>
              ) : (
                <span className="crumbs__current" aria-current={last ? "page" : undefined}>
                  {item.label}
                </span>
              )}
              {!last && <span className="crumbs__sep">‹</span>}
            </li>
          );
        })}
      </ol>
    </nav>
  );
}
