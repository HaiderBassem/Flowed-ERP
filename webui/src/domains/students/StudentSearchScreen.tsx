import { useEffect, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal } from "@/api/errors";
import type { Page, StudentView } from "@/api/types";
import { useStudyTypes } from "@/api/reference";
import { RefusalPanel } from "@/components/RefusalPanel";
import { StudyTypeChip } from "@/components/Chip";
import { EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { useWorkingContext } from "@/app/working-context";
import { fold, formatPhone } from "@/lib/text";
import { labelStudentStatus } from "@/design/lexicon";

/**
 * The student index.
 *
 * The mother's name is a permanent column rather than a detail on the record
 * page: after Arabic folding, two students can carry the same name, and the
 * mother's name is what tells them apart. §06 shows exactly that case.
 *
 * The scope note is always present. A silently filtered list is how a clerk
 * concludes a student does not exist when they are simply in another college.
 */
export function StudentSearchScreen() {
  const [params, setParams] = useSearchParams();
  const { user, can } = useSession();
  const { years } = useWorkingContext();
  const studyTypes = useStudyTypes();
  const [term, setTerm] = useState(params.get("q") ?? "");
  const [academicYearId, setAcademicYearId] = useState(params.get("academic_year_id") ?? "");
  const [studyTypeId, setStudyTypeId] = useState(params.get("study_type_id") ?? "");
  const [stage, setStage] = useState(params.get("stage") ?? "");
  const debounced = useDebounced(term, 250);

  useEffect(() => {
    const next = new URLSearchParams(params);
    if (debounced) next.set("q", debounced);
    else next.delete("q");
    if (academicYearId) next.set("academic_year_id", academicYearId);
    else next.delete("academic_year_id");
    if (studyTypeId) next.set("study_type_id", studyTypeId);
    else next.delete("study_type_id");
    if (stage) next.set("stage", stage);
    else next.delete("stage");
    setParams(next, { replace: true });
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [debounced, academicYearId, studyTypeId, stage]);

  const filtersActive = Boolean(academicYearId || studyTypeId || stage);

  const results = useQuery({
    queryKey: ["students", "index", debounced, academicYearId, studyTypeId, stage],
    queryFn: () =>
      api.get<Page<StudentView>>("/students", {
        query: {
          q: debounced,
          limit: 50,
          ...(academicYearId ? { academic_year_id: academicYearId } : {}),
          ...(studyTypeId ? { study_type_id: studyTypeId } : {}),
          ...(stage ? { stage } : {}),
        },
      }),
    enabled: debounced.trim().length >= 2 || filtersActive,
  });

  const rows = results.data?.data ?? [];
  const scoped = user?.scope_mode && user.scope_mode !== "all" && user.scope_mode !== "unrestricted";

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">الطلبة</h1>
        <span className="grow" />
        {can("student.register") && (
          <Link className="btn btn--primary" to="/students/new">
            تسجيل طالب جديد
          </Link>
        )}
      </div>

      <Panel>
        <input
          className="input"
          autoFocus
          placeholder="رقم جامعي أو اسم أو آخر 4 أرقام هاتف…"
          value={term}
          onChange={(e) => setTerm(e.target.value)}
        />
        <div className="cols cols--thirds" style={{ marginTop: 10 }}>
          <label className="field">
            <span className="field__label">السنة الدراسية</span>
            <select
              className="input"
              value={academicYearId}
              onChange={(e) => setAcademicYearId(e.target.value)}
            >
              <option value="">الكل</option>
              {years.map((y) => (
                <option key={y.id} value={y.id}>
                  {y.code}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">نوع الدراسة</span>
            <select
              className="input"
              value={studyTypeId}
              onChange={(e) => setStudyTypeId(e.target.value)}
            >
              <option value="">الكل</option>
              {(studyTypes.data ?? []).map((t) => (
                <option key={t.id} value={t.id}>
                  {t.name_ar}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">المرحلة</span>
            <select className="input" value={stage} onChange={(e) => setStage(e.target.value)}>
              <option value="">الكل</option>
              {[1, 2, 3, 4, 5].map((n) => (
                <option key={n} value={n}>
                  {n}
                </option>
              ))}
            </select>
          </label>
        </div>
        {debounced.trim().length >= 2 && fold(debounced) !== debounced.trim() && (
          <p className="note" style={{ marginTop: 6 }}>
            طُبِّع البحث إلى <span className="ltr">{fold(debounced)}</span> — النتائج تشمل الصيغتين
            (تشكيل، همزات، ة/ه، ى/ي)
          </p>
        )}
        {scoped && (
          <p className="note" style={{ marginTop: 6 }}>
            معروض ضمن نطاقك التنظيمي: {user!.scope_mode} — لا فلترة صامتة.
          </p>
        )}
      </Panel>

      <div style={{ marginTop: 12 }}>
        {results.isLoading && <Skeleton height={200} />}

        {results.isError &&
          (isRefusal(results.error) ? (
            <RefusalPanel refusal={results.error} onRetry={() => void results.refetch()} />
          ) : (
            <EmptyState kind="no-results" title="تعذّر البحث" />
          ))}

        {debounced.trim().length < 2 && !filtersActive && (
          <EmptyState kind="not-yet" title="اكتب حرفين على الأقل للبحث، أو استخدم الفلاتر" />
        )}

        {results.isSuccess && rows.length === 0 && (
          <EmptyState
            kind="no-results"
            title="لا نتائج لهذا البحث"
            detail="جرّب رقماً جامعياً، أو آخر أربعة أرقام من الهاتف."
          />
        )}

        {rows.length > 0 && (
          <div className="table-wrap">
            <table className="grid">
              <thead>
                <tr>
                  <th>الرقم الجامعي</th>
                  <th>الاسم</th>
                  <th>اسم الأم</th>
                  <th>الهاتف</th>
                  <th>نوع الدراسة</th>
                  <th>السنة الدراسية</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((student) => (
                  <tr key={student.id}>
                    <td className="k">
                      <Link to={`/students/${student.id}`}>{student.student_no}</Link>
                    </td>
                    <td>
                      <Link to={`/students/${student.id}`}>{student.full_name}</Link>
                    </td>
                    <td>{student.mother_name}</td>
                    <td className="num">{formatPhone(student.phone)}</td>
                    <td>
                      <StudyTypeChip code={student.current_study_type_code} />
                    </td>
                    <td className="num">
                      {years.find((y) => y.id === student.current_academic_year_id)?.code ?? "—"}
                    </td>
                    <td>{labelStudentStatus(student.status)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}

        {results.data && results.data.total > rows.length && (
          <p className="note" style={{ marginTop: 8 }}>
            معروض {rows.length} من {results.data.total} — ضيّق البحث لرؤية البقية.
          </p>
        )}
      </div>
    </main>
  );
}

function useDebounced<T>(value: T, ms: number): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const id = window.setTimeout(() => setDebounced(value), ms);
    return () => window.clearTimeout(id);
  }, [value, ms]);
  return debounced;
}
