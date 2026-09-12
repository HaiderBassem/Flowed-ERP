import { useEffect, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { IdentityVersionView, MergeStudentsResponse, Page, StudentView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDate, formatDateTime } from "@/lib/dates";

/**
 * Legal identity — the versions of who this person officially is.
 *
 * Iraqi courts change names and civil-registry details, and a certificate
 * issued before the change must keep naming the person as it named them. So
 * identity is versioned, nothing here is editable, and every change carries
 * the court decision that ordered it.
 *
 * The merge lives here too, and it is the most dangerous command on the
 * student file: merging two people is far worse than leaving two records for
 * one person, which is why disagreeing identities demand an explicit
 * acknowledgement in words.
 */
export function IdentityScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();

  const student = useQuery({
    queryKey: ["student", id],
    queryFn: () => api.get<StudentView>(`/students/${id}`),
    enabled: Boolean(id),
  });

  const history = useQuery({
    queryKey: ["student-identity", id],
    queryFn: () => api.get<IdentityVersionView[]>(`/students/${id}/identity-history`),
    enabled: Boolean(id) && can("identity.read"),
    retry: false,
  });

  if (student.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={200} />
      </main>
    );
  }

  const view = student.data;
  if (!view) {
    return (
      <main className="screen">
        <EmptyState kind="no-results" title="تعذّر فتح الطالب" />
      </main>
    );
  }

  return (
    <main className="screen">
      <Crumbs
        items={[
          { label: "الطلبة", to: "/students" },
          { label: view.full_name, to: `/students/${view.id}` },
          { label: "الهوية القانونية" },
        ]}
      />
      <div className="screen__head">
        <h1 className="screen__title">الهوية القانونية — {view.full_name}</h1>
        <span className="num">{view.student_no}</span>
        <span className="grow" />
        <Link className="btn" to={`/students/${view.id}`}>
          ملف الطالب
        </Link>
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <div className="stack">
          <Panel title="سجل الإصدارات" aside={<span className="label">لا شيء هنا يُعدَّل</span>}>
            {!can("identity.read") ? (
              <EmptyState
                kind="forbidden"
                title="سجل الهوية للمسجّل والمدقق"
                detail={reason("identity.read")}
              />
            ) : history.isLoading ? (
              <Skeleton height={120} />
            ) : (history.data ?? []).length === 0 ? (
              <EmptyState
                kind="not-yet"
                title="نسخة واحدة — لا تغييرات قضائية"
                detail="الاسم كما سُجّل أول مرة ما زال هو الهوية القانونية."
              />
            ) : (
              <ul className="timeline">
                {(history.data ?? []).map((version) => (
                  <li className="timeline__item" key={version.version_no}>
                    <span className="timeline__dot" data-tone="frozen" />
                    <span className="timeline__when">
                      {formatDateTime(version.recorded_at)}
                    </span>{" "}
                    <b>الإصدار {version.version_no}</b> — {version.full_name}
                    <div className="note">
                      الأم: {version.mother_name}
                      {version.national_id && (
                        <>
                          {" "}
                          · وطني <span className="num">{version.national_id}</span>
                        </>
                      )}
                      {version.court_decision_no && (
                        <>
                          {" "}
                          · قرار محكمة <span className="ltr num">{version.court_decision_no}</span>
                          {version.court_decision_date &&
                            ` في ${formatDate(version.court_decision_date)}`}
                        </>
                      )}
                      {" — "}
                      {version.reason}
                    </div>
                  </li>
                ))}
              </ul>
            )}
          </Panel>

          <IdentityChangeForm studentId={view.id} current={view} />
        </div>

        <MergePanel target={view} />
      </div>
    </main>
  );
}

function IdentityChangeForm({
  studentId,
  current,
}: {
  studentId: string;
  current: StudentView;
}) {
  const queryClient = useQueryClient();
  const { can, reason } = useSession();
  const [fullName, setFullName] = useState("");
  const [motherName, setMotherName] = useState("");
  const [courtNo, setCourtNo] = useState("");
  const [courtDate, setCourtDate] = useState("");
  const [changeReason, setChangeReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const record = useMutation({
    mutationFn: () =>
      api.post(`/students/${studentId}/identity`, {
        ...(fullName.trim() ? { full_name: fullName.trim() } : {}),
        ...(motherName.trim() ? { mother_name: motherName.trim() } : {}),
        court_decision_no: courtNo.trim(),
        court_decision_date: courtDate,
        reason: changeReason.trim(),
      }),
    onSuccess: () => {
      setFullName("");
      setMotherName("");
      setCourtNo("");
      setChangeReason("");
      void queryClient.invalidateQueries({ queryKey: ["student", studentId] });
      void queryClient.invalidateQueries({ queryKey: ["student-identity", studentId] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const changed = Boolean(fullName.trim() || motherName.trim());
  const ready = changed && courtNo.trim() && courtDate && changeReason.trim().length >= 3;

  return (
    <Panel title="تسجيل تغيير قضائي">
      <p className="note" style={{ marginBottom: 10 }}>
        التغيير لا يمحو شيئاً: يُنشئ إصداراً جديداً، والوثائق الصادرة قبل القرار تبقى تسمّي
        الشخص كما سمّته.
      </p>

      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">الاسم الجديد</span>
          <input
            className="input"
            placeholder={current.full_name}
            value={fullName}
            onChange={(e) => setFullName(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field__label">اسم الأم الجديد</span>
          <input
            className="input"
            placeholder={current.mother_name}
            value={motherName}
            onChange={(e) => setMotherName(e.target.value)}
          />
        </label>
      </div>

      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">رقم قرار المحكمة (إلزامي)</span>
          <input
            className="input ltr"
            value={courtNo}
            onChange={(e) => setCourtNo(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field__label">تاريخ القرار (إلزامي)</span>
          <input
            className="input num"
            type="date"
            value={courtDate}
            onChange={(e) => setCourtDate(e.target.value)}
          />
        </label>
      </div>

      <label className="field">
        <span className="field__label">السبب</span>
        <input
          className="input"
          value={changeReason}
          onChange={(e) => setChangeReason(e.target.value)}
        />
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <Button
        variant="primary"
        disabled={!can("identity.write") || !ready}
        disabledReason={
          !can("identity.write")
            ? reason("identity.write")
            : !changed
              ? "غيّر الاسم أو اسم الأم"
              : "رقم القرار وتاريخه والسبب إلزامية"
        }
        busy={record.isPending}
        onClick={() => record.mutate()}
      >
        تسجيل الإصدار الجديد
      </Button>
    </Panel>
  );
}

function MergePanel({ target }: { target: StudentView }) {
  const navigate = useNavigate();
  const { can, reason } = useSession();
  // The duplicate is found the way a person finds a student — by number or
  // name — not by pasting a UUID from an address bar. The identifier stays
  // internal; the operator confirms a face: name, number, mother's name.
  const [term, setTerm] = useState("");
  const [source, setSource] = useState<StudentView | null>(null);
  const [mergeReason, setMergeReason] = useState("");
  const [acknowledge, setAcknowledge] = useState(false);
  const [result, setResult] = useState<MergeStudentsResponse | null>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const debounced = useDebounced(term, 250);
  const candidates = useQuery({
    queryKey: ["students", "merge-search", debounced],
    queryFn: () => api.get<Page<StudentView>>("/students", { query: { q: debounced, limit: 6 } }),
    enabled: debounced.trim().length >= 2 && !source,
  });
  const sourceId = source?.id ?? "";

  const merge = useMutation({
    mutationFn: () =>
      api.post<MergeStudentsResponse>(`/students/${target.id}/merge`, {
        source_student_id: sourceId.trim(),
        reason: mergeReason.trim(),
        ...(acknowledge ? { acknowledge_different_identity: true } : {}),
      }),
    onSuccess: (response) => {
      setResult(response);
      setRefusal(null);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const identityMismatch =
    refusal !== null && /identity|different/i.test(refusal.code);

  if (result) {
    return (
      <Panel title="اكتمل الدمج">
        <Row label="انتقل إلى هذا السجل">
          <span className="num">{result.enrollments_moved}</span> تسجيلاً ·{" "}
          <span className="num">{result.accounts_moved}</span> حساباً ·{" "}
          <span className="num">{result.discounts_moved}</span> خصماً
        </Row>
        <p className="note" style={{ marginTop: 8 }}>
          السجل المصدر لم يُحذف — لا حذف في النظام — بل أُقفل وأُشّر بالدمج، وسجل التدقيق يحمل
          القرار باسم من اتخذه.
        </p>
        <Button variant="ghost" onClick={() => navigate(0)}>
          تحديث الصفحة
        </Button>
      </Panel>
    );
  }

  return (
    <Panel
      title={
        <>
          دمج سجل مكرر <Chip tone="void">أخطر أمر في هذا الملف</Chip>
        </>
      }
    >
      <p className="note" style={{ marginBottom: 10 }}>
        يطوي سجلاً مكرراً <b>داخل هذا السجل</b>: تسجيلاته وحساباته وخصومه تنتقل إلى{" "}
        <b>{target.full_name}</b>. دمج شخصين مختلفين أسوأ بكثير من ترك سجلّين لشخص واحد — لهذا
        يُرفض الدمج حين تختلف الهويتان إلا بإقرار صريح.
      </p>

      {!source ? (
        <label className="field">
          <span className="field__label">السجل المكرر (الذي سيُطوى) — ابحث برقم أو اسم</span>
          <input
            className="input"
            value={term}
            onChange={(e) => setTerm(e.target.value)}
            placeholder="رقم جامعي أو اسم…"
          />
          <div style={{ marginTop: 6 }}>
            {(candidates.data?.data ?? [])
              .filter((s) => s.id !== target.id)
              .map((s) => (
                <button
                  key={s.id}
                  type="button"
                  className="palette__row"
                  style={{ borderRadius: 6 }}
                  onClick={() => setSource(s)}
                >
                  <span className="grow">
                    <b>{s.full_name}</b>{" "}
                    <span className="label">— الأم: {s.mother_name}</span>
                  </span>
                  <span className="num">{s.student_no}</span>
                </button>
              ))}
          </div>
        </label>
      ) : (
        <div className="callout" style={{ marginBottom: 12 }}>
          <div className="row">
            <span className="grow">
              سيُطوى: <b>{source.full_name}</b>{" "}
              <span className="label">— الأم: {source.mother_name}</span>{" "}
              <span className="num">{source.student_no}</span>
            </span>
            <Button size="sm" variant="ghost" onClick={() => setSource(null)}>
              تغيير
            </Button>
          </div>
        </div>
      )}

      <label className="field">
        <span className="field__label">السبب (إلزامي، ويُسجَّل باسمك)</span>
        <input
          className="input"
          value={mergeReason}
          onChange={(e) => setMergeReason(e.target.value)}
        />
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel
            refusal={refusal}
            {...(identityMismatch
              ? {
                  actions: [
                    {
                      label: "الهويتان لشخص واحد فعلاً — أُقرّ وأدمج",
                      primary: true,
                      onClick: () => {
                        setAcknowledge(true);
                        merge.mutate();
                      },
                    },
                  ],
                }
              : {})}
          />
        </div>
      )}

      <Button
        variant="danger"
        disabled={!can("identity.write") || !sourceId.trim() || mergeReason.trim().length < 3}
        disabledReason={
          !can("identity.write") ? reason("identity.write") : "المصدر والسبب إلزاميان"
        }
        busy={merge.isPending}
        onClick={() => merge.mutate()}
      >
        دمج في هذا السجل
      </Button>
    </Panel>
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
