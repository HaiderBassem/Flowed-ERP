import { Link } from "react-router-dom";

import { Panel } from "@/components/primitives";
import { useWorkingContext } from "@/app/working-context";
import { REPORTS } from "./catalogue";

/** The report centre — §09. */
export function ReportsScreen() {
  const { activeYear } = useWorkingContext();

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">التقارير</h1>
        {activeYear && <span className="num">{activeYear.code}</span>}
      </div>

      <div className="callout callout--note" style={{ marginTop: 10 }}>
        كل تقرير يحمل <b>ترويسة سياق</b> تُطبع وتُصدَّر معه: السنة، النطاق، الفلاتر، لحظة
        القراءة، ومن أخرجه. تقرير بلا ترويسته ورقة مجهولة المصدر في اجتماع.
      </div>

      <div className="cols cols--thirds" style={{ marginTop: 12 }}>
        {REPORTS.filter((r) => r.id !== "statement").map((report) => (
          <Link key={report.id} to={`/reports/${report.id}`} style={{ textDecoration: "none" }}>
            <Panel title={report.label}>
              <p className="note" style={{ margin: 0 }}>
                {report.description}
              </p>
              {report.dateRange && (
                <p className="note" style={{ marginTop: 6 }}>
                  محدود بمدى تاريخي لا بسنة — يوم الصراف يخص وردية لا سنة دراسية.
                </p>
              )}
            </Panel>
          </Link>
        ))}
      </div>
    </main>
  );
}
