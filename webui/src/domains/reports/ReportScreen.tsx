import { useMemo, useState } from "react";
import { Link, useParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { departmentsOf, useColleges, useDepartments, useStudyTypes } from "@/api/reference";
import type { YearView } from "@/api/types";
import { Money } from "@/components/Money";
import { Crumbs } from "@/components/Crumbs";
import { formatCount } from "@/lib/money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { formatDateTime } from "@/lib/dates";
import { REPORTS, type ReportSpec } from "./catalogue";
import { labelFor, NOISY_COLUMNS } from "./columns";

/** What the filter bar holds. Every field maps to a filter the handler reads. */
interface ReportFilters {
  college_id?: string;
  department_id?: string;
  study_type_id?: string;
  stage?: string;
  from?: string;
  to?: string;
}

/**
 * A report, rendered generically from what the server returns.
 *
 * Two of §11's rules are structural here and not left to each report:
 *
 *   - the **context header** is emitted with every report and goes to the
 *     printer and the export with it. A figure quoted in a meeting without its
 *     year is a figure nobody can check.
 *   - **counts and money are visually separated** by a rule and carry a
 *     permanent footnote, because they come from different populations on
 *     purpose — counts from effective enrollments, money from every
 *     non-cancelled account. Dividing one by the other produces an average
 *     that means nothing, and someone then makes a decision on it.
 *
 * The year is mandatory and the request is not sent without one.
 */
/**
 * Remounts on every report change.
 *
 * The filters are screen state, and two reports do not share them: carrying a
 * department from the debt list into a report that has no department filter
 * leaves a select showing a narrowing the figures were not run under.
 */
export function KeyedReportScreen() {
  const { report } = useParams<{ report: string }>();
  return <ReportScreen key={report} />;
}

export function ReportScreen() {
  const { report: reportId } = useParams<{ report: string }>();
  const { activeYear } = useWorkingContext();
  const { user } = useSession();
  const [readAt] = useState(() => new Date());
  // Today, where the server refuses without a range. The drawer sheet is read
  // at the end of a shift far more often than for any other day, and an empty
  // form that refuses on submit teaches nothing the field could not have said.
  const [filters, setFilters] = useState<ReportFilters>(() => {
    if (!REPORTS.find((r) => r.id === reportId)?.datesRequired) return {};
    const today = new Date().toISOString().slice(0, 10);
    return { from: today, to: today };
  });

  // Cached long and shared with every other screen; naming the selected
  // department on the printed page needs the names, not the identifiers.
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const spec = REPORTS.find((r) => r.id === reportId);

  const path = spec?.id === "year" ? `/reports/years/${activeYear?.id}` : spec?.path;

  // The statement is one student's document and is reached from their file;
  // there is no whole-university version of it to run from here.
  const perStudent = spec?.id === "statement";

  const query = useMemo(
    () => (spec ? reportQuery(spec, activeYear, filters) : {}),
    [spec, activeYear, filters],
  );

  // The dates are as mandatory as the year where the server says so, and for
  // the same reason: they are what bounds the scan. Sending the request
  // without them buys a refusal the operator has to read to learn what the
  // form could have told them.
  const datesGiven = !spec?.datesRequired || Boolean(filters.from && filters.to);

  const enabled =
    Boolean(spec) && !perStudent && (!spec!.yearRequired || Boolean(activeYear)) && datesGiven;

  const data = useQuery({
    // The filters are part of the key. Without them the cache answers a
    // narrowed request with the previous department's figures, under the new
    // department's heading.
    queryKey: ["report", spec?.id, activeYear?.id, query],
    queryFn: () => api.get<unknown>(path!, { query: { ...query, limit: 500 } }),
    enabled,
  });

  if (!spec) {
    return (
      <main className="screen">
        <EmptyState kind="no-results" title="تقرير غير معروف" />
      </main>
    );
  }

  return (
    <main className="screen">
      <Crumbs items={[{ label: "التقارير", to: "/reports" }, { label: spec.label }]} />
      <div className="screen__head">
        <h1 className="screen__title">{spec.label}</h1>
        <span className="grow" />
        <Link className="btn no-print" to="/reports">
          كل التقارير
        </Link>
        {enabled && <ExportButtons path={path!} query={query} />}
        <Button onClick={() => window.print()}>طباعة</Button>
      </div>

      {perStudent && (
        <EmptyState
          kind="not-yet"
          title="كشف الطالب يُفتح من ملف الطالب"
          detail="الكشف وثيقة طالب واحد ولا نسخة جامعية منه. افتح ملف الطالب ثم «كشف الطالب» — ومن هناك يُصدَّر أيضاً."
        />
      )}

      {!perStudent && <FilterBar spec={spec} filters={filters} onChange={setFilters} />}

      {/* Printed and exported with the report, always. */}
      <dl className="report-context">
        <div>
          <dt>السنة:</dt>
          <dd className="num">{activeYear?.code ?? "—"}</dd>
        </div>
        <div>
          <dt>النطاق:</dt>
          <dd>{user?.scope_mode && user.scope_mode !== "all" ? user.scope_mode : "الجامعة كاملة"}</dd>
        </div>
        {/* The narrowing is part of the context, not part of the controls: a
            printed page whose filters live only in the form above it is a page
            that will be read as the whole university's figures. */}
        <div>
          <dt>التضييق:</dt>
          <dd>{narrowingText(spec, filters, colleges.data, departments.data, studyTypes.data)}</dd>
        </div>
        <div>
          <dt>لحظة القراءة:</dt>
          <dd className="num">{formatDateTime(readAt)}</dd>
        </div>
        <div>
          <dt>أخرجه:</dt>
          <dd>{user?.full_name ?? "—"}</dd>
        </div>
      </dl>

      {/* The year is what bounds the scan; without one no request is made. */}
      {spec.yearRequired && !activeYear && (
        <EmptyState
          kind="not-yet"
          title="اختر السنة أولاً"
          detail="السنة إلزامية في كل تقرير مالي: هي ما يحدّ المسح، وأرقام سنتين غير قابلة للمقارنة أصلاً."
        />
      )}

      {!perStudent && !datesGiven && (
        <EmptyState
          kind="not-yet"
          title="حدّد المدة أولاً"
          detail="يوم الصرّاف يخصّ وردية لا سنة دراسية، والمدة هي ما يحدّ المسح — لذلك «من» و«إلى» إلزاميتان هنا."
        />
      )}

      {enabled && data.isLoading && <Skeleton height={240} />}

      {data.isError &&
        (isRefusal(data.error) ? (
          <RefusalPanel refusal={data.error} onRetry={() => void data.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر تشغيل التقرير" />
        ))}

      {data.isSuccess && <ReportTable payload={data.data} spec={spec} />}
    </main>
  );
}

/**
 * Renders whatever the report returned.
 *
 * Generic rather than fifteen bespoke tables: every report here is a list of
 * flat rows, and the two things that must be right — the money formatting and
 * the count/money separation — are properties of the columns, not of the
 * report. A bespoke component per report would be fifteen chances to get one
 * of them wrong.
 */
function ReportTable({
  payload,
  spec,
}: {
  payload: unknown;
  spec: { mixesCountAndMoney?: boolean };
}) {
  const rows = extractRows(payload);

  if (rows.length === 0) {
    return (
      <Panel>
        <EmptyState kind="no-results" title="لا صفوف لهذه الفلاتر" />
      </Panel>
    );
  }

  const allColumns = Object.keys(rows[0]!).filter((c) => !NOISY_COLUMNS.has(c));

  // A column holding an array of rows is the report's own drill-down — the
  // departments inside a college, the stages inside a department. §11 requires
  // every aggregate to open to the rows beneath it, so it becomes an expandable
  // sub-table rather than being stringified into "[object Object]".
  const nested = allColumns.filter((c) => isRowArray(rows[0]![c]));
  const columns = allColumns.filter((c) => !nested.includes(c));
  const moneyColumns = new Set(columns.filter(isMoneyColumn));
  const firstMoney = columns.find((c) => moneyColumns.has(c));

  return (
    <>
      <div className="table-wrap">
        <table className="grid">
          <thead>
            <tr>
              {nested.length > 0 && <th aria-label="فتح" style={{ width: 32 }} />}
              {columns.map((column) => (
                <th
                  key={column}
                  className={headingClass(column, moneyColumns, firstMoney)}
                  title={column}
                >
                  {labelFor(column)}
                </th>
              ))}
            </tr>
          </thead>
          <tbody>
            {rows.map((row, index) => (
              <ReportRow
                key={index}
                row={row}
                columns={columns}
                nested={nested}
                moneyColumns={moneyColumns}
                firstMoney={firstMoney}
              />
            ))}
          </tbody>
        </table>
      </div>

      {spec.mixesCountAndMoney && (
        <p className="note" style={{ marginTop: 8, maxWidth: "72ch" }}>
          <b>حاشية دائمة:</b> أعمدة العدد تأتي من التسجيلات الفعّالة، وأعمدة المال من كل
          الحسابات غير الملغاة — مجتمعا بيانات مختلفان عمداً. الفرق بينهما حسابات استبدال تحمل
          نقداً حقيقياً، ولذلك <b>قسمة المال على العدد لا تعطي متوسطاً ذا معنى</b>.
        </p>
      )}
    </>
  );
}

/** One row, and the sub-rows it opens to. */
function ReportRow({
  row,
  columns,
  nested,
  moneyColumns,
  firstMoney,
}: {
  row: Record<string, unknown>;
  columns: string[];
  nested: string[];
  moneyColumns: Set<string>;
  firstMoney: string | undefined;
}) {
  const [open, setOpen] = useState(false);
  const children = nested.flatMap((key) => (row[key] as Record<string, unknown>[]) ?? []);

  return (
    <>
      <tr {...(nested.length > 0 ? { "data-clickable": "" } : {})}>
        {nested.length > 0 && (
          <td>
            <button
              type="button"
              className="btn btn--sm btn--ghost"
              onClick={() => setOpen((o) => !o)}
              aria-expanded={open}
              title={`${children.length} صفاً تفصيلياً`}
            >
              {open ? "−" : "+"}
            </button>
          </td>
        )}
        {columns.map((column) => (
          <ReportCell
            key={column}
            column={column}
            value={row[column]}
            moneyColumns={moneyColumns}
            firstMoney={firstMoney}
          />
        ))}
      </tr>
      {open && children.length > 0 && (
        <tr>
          <td colSpan={columns.length + 1} style={{ padding: 0, background: "var(--surface-2)" }}>
            <ReportTable payload={children} spec={{}} />
          </td>
        </tr>
      )}
    </>
  );
}

function ReportCell({
  column,
  value,
  moneyColumns,
  firstMoney,
}: {
  column: string;
  value: unknown;
  moneyColumns: Set<string>;
  firstMoney: string | undefined;
}) {
  if (moneyColumns.has(column)) {
    return (
      <td className={column === firstMoney ? "n col-group-money" : "n"}>
        <Money value={typeof value === "number" ? value : null} tone="plain" />
      </td>
    );
  }
  if (typeof value === "number") {
    // A rate is not money and is not a count; it keeps its decimals and its
    // sign-free rendering so it cannot be mistaken for either.
    const isRate = /_pct$|_rate$/.test(column);
    return <td className="n">{isRate ? value.toFixed(2) : formatCount(value)}</td>;
  }
  if (typeof value === "boolean") {
    return <td>{value ? "نعم" : "لا"}</td>;
  }
  return <td>{value === null || value === undefined ? "—" : String(value)}</td>;
}

/** True when a value is an array of row-shaped objects — a nested report. */
function isRowArray(value: unknown): boolean {
  return (
    Array.isArray(value) &&
    value.length > 0 &&
    typeof value[0] === "object" &&
    value[0] !== null &&
    !Array.isArray(value[0])
  );
}

/**
 * The rule between counts and money is drawn on the first money column, so the
 * two groups are visibly separated wherever a report carries both.
 */
function headingClass(
  column: string,
  moneyColumns: Set<string>,
  firstMoney: string | undefined,
): string | undefined {
  if (column === firstMoney) return "n col-group-money";
  if (moneyColumns.has(column)) return "n";
  return undefined;
}

function extractRows(payload: unknown): Record<string, unknown>[] {
  if (Array.isArray(payload)) return payload as Record<string, unknown>[];
  if (payload && typeof payload === "object") {
    const record = payload as Record<string, unknown>;
    if (Array.isArray(record["data"])) return record["data"] as Record<string, unknown>[];
    for (const value of Object.values(record)) {
      if (Array.isArray(value) && value.length > 0 && typeof value[0] === "object") {
        return value as Record<string, unknown>[];
      }
    }
    // A single summary object is one row.
    return [record];
  }
  return [];
}

/**
 * The filter bar — only the dimensions this report's handler actually reads.
 *
 * A select the server ignores is worse than an absent one: an officer narrows
 * to one department, gets the whole university back, and files it as that
 * department's figures. `spec.dimensions` is the list of filters the handler
 * really applies, so what is shown here and what bounds the query are the same
 * list.
 */
function FilterBar({
  spec,
  filters,
  onChange,
}: {
  spec: ReportSpec;
  filters: ReportFilters;
  onChange: (next: ReportFilters) => void;
}) {
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();

  const dimensions = spec.dimensions ?? [];
  if (dimensions.length === 0 && !spec.dateRange) return null;

  const set = (patch: ReportFilters) => onChange({ ...filters, ...patch });
  const narrowed = Object.values(filters).some((value) => value);

  return (
    <div className="no-print">
      <Panel>
        <div className="filter-bar">
        {dimensions.includes("college") && (
          <label className="field">
            <span className="field__label">الكلية</span>
            <select
              className="input"
              value={filters.college_id ?? ""}
              // The department is cleared with the college. Keeping it would
              // send a department that belongs to another college, and the
              // report would come back empty for no visible reason.
              onChange={(e) =>
                set({ college_id: e.target.value || undefined, department_id: undefined })
              }
            >
              <option value="">كل الكليات</option>
              {(colleges.data ?? []).map((college) => (
                <option key={college.id} value={college.id}>
                  {college.name_ar}
                </option>
              ))}
            </select>
          </label>
        )}

        {dimensions.includes("department") && (
          <label className="field">
            <span className="field__label">القسم</span>
            <select
              className="input"
              value={filters.department_id ?? ""}
              onChange={(e) => set({ department_id: e.target.value || undefined })}
              disabled={!filters.college_id}
            >
              <option value="">
                {filters.college_id ? "كل الأقسام" : "اختر الكلية أولاً"}
              </option>
              {departmentsOf(departments.data, filters.college_id).map((department) => (
                <option key={department.id} value={department.id}>
                  {department.name_ar}
                </option>
              ))}
            </select>
          </label>
        )}

        {dimensions.includes("study_type") && (
          <label className="field">
            <span className="field__label">نوع الدراسة</span>
            <select
              className="input"
              value={filters.study_type_id ?? ""}
              onChange={(e) => set({ study_type_id: e.target.value || undefined })}
            >
              <option value="">كل الأنواع</option>
              {(studyTypes.data ?? []).map((type) => (
                <option key={type.id} value={type.id}>
                  {type.name_ar}
                </option>
              ))}
            </select>
          </label>
        )}

        {dimensions.includes("stage") && (
          <label className="field">
            <span className="field__label">المرحلة</span>
            <input
              className="input num"
              type="number"
              min={1}
              max={7}
              value={filters.stage ?? ""}
              placeholder="كل المراحل"
              onChange={(e) => set({ stage: e.target.value || undefined })}
            />
          </label>
        )}

        {spec.dateRange && (
          <>
            <label className="field">
              <span className="field__label">من</span>
              <input
                className="input num"
                type="date"
                value={filters.from ?? ""}
                onChange={(e) => set({ from: e.target.value || undefined })}
              />
            </label>
            <label className="field">
              <span className="field__label">إلى</span>
              <input
                className="input num"
                type="date"
                value={filters.to ?? ""}
                onChange={(e) => set({ to: e.target.value || undefined })}
              />
            </label>
          </>
        )}

          {narrowed && (
            <Button variant="ghost" onClick={() => onChange({})}>
              مسح التضييق
            </Button>
          )}
        </div>
      </Panel>
    </div>
  );
}

/**
 * The two downloads.
 *
 * They carry the filters the screen is showing, so the file and the page agree
 * — an export that silently ran unfiltered is a spreadsheet somebody quotes as
 * one department's figures.
 *
 * The page's own طباعة button covers print, so there is no third button for
 * the server's print-ready page: two ways to print one report is two pages
 * that can disagree.
 *
 * An export is the whole result rather than the page on screen, which is the
 * server's rule and not this button's — the limit is dropped from the query so
 * nothing here can quietly cap it.
 */
function ExportButtons({
  path,
  query,
}: {
  path: string;
  query: Record<string, string | number | undefined>;
}) {
  const [running, setRunning] = useState<string | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const run = async (format: "csv" | "xlsx") => {
    setRunning(format);
    setRefusal(null);
    try {
      await api.download(path, { ...query, format });
    } catch (error) {
      // A refusal on an export arrives as JSON on a route that normally
      // returns a file. Shown rather than swallowed: a 403 here means the
      // scope refused it, and a button that just stops looks broken.
      if (isRefusal(error)) setRefusal(error);
      else throw error;
    } finally {
      setRunning(null);
    }
  };

  return (
    <>
      <Button variant="ghost" disabled={running !== null} onClick={() => void run("csv")}>
        {running === "csv" ? "…CSV" : "CSV"}
      </Button>
      <Button variant="ghost" disabled={running !== null} onClick={() => void run("xlsx")}>
        {running === "xlsx" ? "…Excel" : "Excel"}
      </Button>
      {refusal && (
        <div className="no-print" style={{ flexBasis: "100%" }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}
    </>
  );
}

/**
 * The query one report runs under.
 *
 * Only the parameters the handler reads are sent. A stray parameter is not
 * harmless here: it changes the cache key without changing the result, so two
 * identical reports are fetched twice and a reader comparing them sees two
 * read timestamps for one figure.
 */
function reportQuery(
  spec: ReportSpec,
  year: YearView | null,
  filters: ReportFilters,
): Record<string, string | number | undefined> {
  const query: Record<string, string | number | undefined> = {};
  // The year summary carries its year in the path, so sending it again would
  // be a filter the handler does not read.
  if (spec.yearRequired && year && spec.id !== "year") query["academic_year_id"] = year.id;

  const dimensions = spec.dimensions ?? [];
  if (dimensions.includes("college")) query["college_id"] = filters.college_id;
  if (dimensions.includes("department")) query["department_id"] = filters.department_id;
  if (dimensions.includes("study_type")) query["study_type_id"] = filters.study_type_id;
  if (dimensions.includes("stage")) query["stage"] = filters.stage;
  if (spec.dateRange) {
    query["from"] = filters.from;
    query["to"] = filters.to;
  }
  return query;
}

/** What the report was narrowed to, in names, for the printed context header. */
function narrowingText(
  spec: ReportSpec,
  filters: ReportFilters,
  colleges: { id: string; name_ar: string }[] | undefined,
  departments: { id: string; name_ar: string }[] | undefined,
  studyTypes: { id: string; name_ar: string }[] | undefined,
): string {
  const parts: string[] = [];
  const named = (rows: { id: string; name_ar: string }[] | undefined, id?: string) =>
    id ? (rows?.find((row) => row.id === id)?.name_ar ?? id) : undefined;

  const college = named(colleges, filters.college_id);
  if (college) parts.push(college);
  const department = named(departments, filters.department_id);
  if (department) parts.push(department);
  const studyType = named(studyTypes, filters.study_type_id);
  if (studyType) parts.push(studyType);
  if (filters.stage) parts.push(`المرحلة ${filters.stage}`);
  if (spec.dateRange && filters.from) parts.push(`من ${filters.from}`);
  if (spec.dateRange && filters.to) parts.push(`إلى ${filters.to}`);

  return parts.length > 0 ? parts.join(" · ") : "بلا تضييق";
}

/** Column names the server uses for money. Counts deliberately excluded. */
function isMoneyColumn(name: string): boolean {
  return /(^|_)(amount|total|net|paid|gross|remaining|outstanding|discount|refunded|credit|collected|obligation|balance|expected|variance|float)($|_)/.test(
    name,
  );
}
