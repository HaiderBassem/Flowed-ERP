import { Link, useLocation } from "react-router-dom";

import { EmptyState, Panel } from "./primitives";

/**
 * A route that exists in the specification but is not built in this pass.
 *
 * It says so plainly instead of rendering a plausible-looking empty screen.
 * §07's rule about empty states is the reason: a screen that looks healthy but
 * shows nothing is indistinguishable from a permission fault or a broken
 * query, and each of those needs a different response from whoever hits it.
 */
export function NotBuiltScreen() {
  const location = useLocation();

  return (
    <main className="screen">
      <Panel>
        <EmptyState
          kind="not-yet"
          title="هذه الشاشة غير مبنية في هذه المرحلة"
          detail={
            <div>
              <p className="note">
                المسار <span className="ltr k">{location.pathname}</span> موجود في المواصفة ولم
                يُبنَ بعد. لا يعرض شيئاً لأن عرض شاشة فارغة تبدو سليمة يخفي فرق بين «لا بيانات»
                و«لا صلاحية» و«عطل».
              </p>
              <p className="note">
                المبني في هذه المرحلة: نظام التصميم، شبّاك القبض والوردية والوصل، ملف الطالب
                وصفحة الحساب، صناديق الإلغاء والاسترجاع، التقارير، والرقابة.
              </p>
            </div>
          }
          action={
            <Link className="btn btn--primary" to="/">
              العودة
            </Link>
          }
        />
      </Panel>
    </main>
  );
}
