import { Link } from "react-router-dom";

import { EmptyState, Panel } from "./primitives";

/**
 * A screen the specification asks for that the API cannot supply.
 *
 * Distinct from "not built": the command exists and works, but nothing
 * enumerates the things waiting for it, so an inbox cannot be assembled
 * without the interface inventing a list. §07's rule about empty states is
 * exactly this — a plausible blank panel would read as "nothing pending",
 * which is a different and much worse claim than "I cannot see what is
 * pending".
 */
export function UnavailableScreen({
  title,
  what,
  missing,
  workaround,
}: {
  title: string;
  what: string;
  missing: string;
  workaround?: { label: string; to: string };
}) {
  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">{title}</h1>
      </div>

      <Panel>
        <EmptyState
          kind="forbidden"
          title="لا يمكن بناء هذه القائمة"
          detail={
            <div style={{ maxWidth: "68ch", margin: "0 auto" }}>
              <p className="note">{what}</p>
              <p className="note">
                <b>الناقص:</b> {missing}
              </p>
              <p className="note">
                لا تُعرض هنا قائمة فارغة لأن «لا شيء معلّق» ادّعاء مختلف تماماً عن «لا أستطيع أن
                أرى ما هو معلّق» — والخلط بينهما يجعل بنداً ينتظر أسبوعاً بلا أن يلاحظه أحد.
              </p>
            </div>
          }
          action={
            workaround ? (
              <Link className="btn btn--primary" to={workaround.to}>
                {workaround.label}
              </Link>
            ) : null
          }
        />
      </Panel>
    </main>
  );
}
