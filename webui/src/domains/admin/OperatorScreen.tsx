import { useMemo, useState } from "react";
import { useParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { useColleges, useDepartments } from "@/api/reference";
import type { LoginAttemptView, Role, SessionView, UserDetailView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { Crumbs } from "@/components/Crumbs";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { ROLE_LABELS, useSession } from "@/app/session";
import { elapsed, formatDateTime } from "@/lib/dates";

const ALL_ROLES: Role[] = [
  "cashier",
  "finance_manager",
  "registrar",
  "academic_officer",
  "auditor",
  "report_viewer",
  "admin",
];

/**
 * One operator: roles, scope, live sessions and failed sign-ins.
 *
 * The rule §09 singles out is on the scope editor: **narrowing a scope shows
 * its consequence before it is saved.** Silently cutting someone off from
 * their work, with no explanation on either side, produces a support call and
 * an operator who believes the system is broken.
 */
export function OperatorScreen() {
  const { id } = useParams<{ id: string }>();
  const { can, reason } = useSession();

  const user = useQuery({
    queryKey: ["user", id],
    queryFn: () => api.get<UserDetailView>(`/users/${id}`),
    enabled: Boolean(id),
  });

  const sessions = useQuery({
    queryKey: ["user-sessions", id],
    queryFn: () => api.get<SessionView[]>(`/users/${id}/sessions`),
    enabled: Boolean(id),
  });

  const attempts = useQuery({
    queryKey: ["user-login-history", id],
    queryFn: () => api.get<LoginAttemptView[]>(`/users/${id}/login-history`),
    enabled: Boolean(id),
  });

  if (user.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="35%" />
        <div style={{ height: 12 }} />
        <Skeleton height={220} />
      </main>
    );
  }

  if (user.isError) {
    return (
      <main className="screen">
        {isRefusal(user.error) ? (
          <RefusalPanel refusal={user.error} onRetry={() => void user.refetch()} />
        ) : (
          <EmptyState kind="no-results" title="تعذّر فتح المستخدم" />
        )}
      </main>
    );
  }

  const view = user.data!;
  const writable = can("operators.administer");

  return (
    <main className="screen">
      <Crumbs
        items={[{ label: "المستخدمون", to: "/admin/operators" }, { label: view.full_name }]}
      />
      <div className="screen__head">
        <h1 className="screen__title">{view.full_name}</h1>
        <span className="k">{view.username}</span>
        {!view.is_active && <Chip tone="void">معطّل</Chip>}
      </div>

      <div className="cols cols--half" style={{ marginTop: 12 }}>
        <div className="stack">
          <RolesPanel user={view} writable={writable} writeReason={reason("operators.administer")} />
          <ScopePanel user={view} writable={writable} writeReason={reason("operators.administer")} />
          <AccountPanel user={view} writable={writable} writeReason={reason("operators.administer")} />
        </div>

        <div className="stack">
          <Panel
            title="الجلسات الحيّة"
            aside={<span className="label">{(sessions.data ?? []).length}</span>}
            flush
          >
            {sessions.isLoading && <Skeleton height={100} />}
            {(sessions.data ?? []).length === 0 && !sessions.isLoading && (
              <EmptyState kind="not-yet" title="لا جلسات مفتوحة" />
            )}
            {(sessions.data ?? []).length > 0 && (
              <table className="grid">
                <thead>
                  <tr>
                    <th>آخر نشاط</th>
                    <th>العنوان</th>
                    <th>تنتهي</th>
                    <th />
                  </tr>
                </thead>
                <tbody>
                  {(sessions.data ?? []).map((session) => (
                    <tr key={session.id}>
                      <td className="num">{formatDateTime(session.last_seen_at)}</td>
                      <td className="num">{session.ip_address ?? "—"}</td>
                      <td className="num">{formatDateTime(session.expires_at)}</td>
                      <td>
                        {session.revoked_at ? (
                          <Chip tone="muted">ملغاة</Chip>
                        ) : (
                          <RevokeButton sessionId={session.id} userId={view.id} writable={writable} />
                        )}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>

          <Panel title="محاولات الدخول" flush>
            {attempts.isLoading && <Skeleton height={100} />}
            {(attempts.data ?? []).length === 0 && !attempts.isLoading && (
              <EmptyState kind="not-yet" title="لا محاولات مسجّلة" />
            )}
            {(attempts.data ?? []).length > 0 && (
              <table className="grid">
                <thead>
                  <tr>
                    <th>الوقت</th>
                    <th>النتيجة</th>
                    <th>السبب</th>
                    <th>العنوان</th>
                  </tr>
                </thead>
                <tbody>
                  {(attempts.data ?? []).slice(0, 20).map((attempt, index) => (
                    <tr key={index}>
                      <td className="num">{formatDateTime(attempt.occurred_at)}</td>
                      <td>
                        {attempt.succeeded ? (
                          <Chip tone="live">نجحت</Chip>
                        ) : (
                          <Chip tone="void">فشلت</Chip>
                        )}
                      </td>
                      <td className="k">{attempt.failure_code ?? "—"}</td>
                      <td className="num">{attempt.ip_address ?? "—"}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </Panel>
        </div>
      </div>
    </main>
  );
}

function RevokeButton({
  sessionId,
  userId,
  writable,
}: {
  sessionId: string;
  userId: string;
  writable: boolean;
}) {
  const queryClient = useQueryClient();
  const revoke = useMutation({
    mutationFn: () => api.post(`/sessions/${sessionId}/revoke`, {}),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["user-sessions", userId] }),
  });
  return (
    <Button
      size="sm"
      variant="danger"
      disabled={!writable}
      busy={revoke.isPending}
      onClick={() => revoke.mutate()}
    >
      إلغاء
    </Button>
  );
}

function RolesPanel({
  user,
  writable,
  writeReason,
}: {
  user: UserDetailView;
  writable: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const [roles, setRoles] = useState<Role[]>(user.roles);
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const save = useMutation({
    mutationFn: () => api.post(`/users/${user.id}/roles`, { roles, reason: reason.trim() }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["user", user.id] });
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      setReason("");
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const changed = useMemo(
    () => roles.slice().sort().join() !== user.roles.slice().sort().join(),
    [roles, user.roles],
  );

  const toggle = (role: Role) =>
    setRoles((current) =>
      current.includes(role) ? current.filter((r) => r !== role) : [...current, role],
    );

  return (
    <Panel title="الأدوار">
      <div className="cluster" style={{ marginBottom: 10 }}>
        {ALL_ROLES.map((role) => (
          <button
            key={role}
            type="button"
            className={`btn${roles.includes(role) ? " btn--primary" : ""}`}
            disabled={!writable}
            title={writable ? undefined : writeReason}
            onClick={() => toggle(role)}
          >
            {ROLE_LABELS[role]}
          </button>
        ))}
      </div>

      {/* Separation of duties, said plainly where it is easiest to break. */}
      {roles.includes("cashier") && roles.includes("finance_manager") && (
        <div className="refusal refusal--pending" style={{ marginBottom: 10 }}>
          <p className="refusal__title">هذا الجمع يُبطل فصل الواجبات</p>
          <p className="refusal__body" style={{ margin: 0 }}>
            صراف ومدير مالي في شخص واحد يستطيع أن يقبض ويلغي ويعتمد درجه بنفسه. النظام يرفض
            الموافقة الذاتية في مواضع، لكنه لا يستطيع تعويض دمج الدورين.
          </p>
        </div>
      )}

      {changed && (
        <label className="field">
          <span className="field__label">سبب التغيير</span>
          <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
      )}

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <Button
        variant="primary"
        disabled={!writable || !changed}
        disabledReason={!writable ? writeReason : !changed ? "لا تغيير" : undefined}
        busy={save.isPending}
        onClick={() => save.mutate()}
      >
        حفظ الأدوار
      </Button>
    </Panel>
  );
}

/**
 * The scope editor, which shows what narrowing costs before it is saved.
 */
function ScopePanel({
  user,
  writable,
  writeReason,
}: {
  user: UserDetailView;
  writable: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const colleges = useColleges();
  const departments = useDepartments();
  const [mode, setMode] = useState(user.scope_mode || "university");
  const [selected, setSelected] = useState<string[]>(user.colleges ?? []);
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const save = useMutation({
    mutationFn: () =>
      api.post(`/users/${user.id}/scope`, {
        scope_mode: mode,
        ...(mode === "scoped" ? { colleges: selected } : {}),
        reason: reason.trim(),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["user", user.id] });
      setReason("");
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const current = user.scope_mode === "scoped" ? (user.colleges ?? []) : null;
  const all = colleges.data ?? [];

  // What this change takes away. Counted from the colleges themselves rather
  // than guessed: the departments figure is exact, and the wording is careful
  // not to claim a student count the interface cannot compute.
  const losing = useMemo(() => {
    const before = current === null ? all.map((c) => c.id) : current;
    const after = mode === "university" ? all.map((c) => c.id) : selected;
    return before.filter((id) => !after.includes(id));
  }, [current, all, mode, selected]);

  const losingDepartments = (departments.data ?? []).filter((d) =>
    losing.includes(d.college_id),
  ).length;

  const changed =
    mode !== (user.scope_mode || "university") ||
    selected.slice().sort().join() !== (user.colleges ?? []).slice().sort().join();

  return (
    <Panel title="النطاق التنظيمي">
      <div className="cluster" style={{ marginBottom: 10 }}>
        <button
          type="button"
          className={`btn${mode === "university" ? " btn--primary" : ""}`}
          disabled={!writable}
          onClick={() => setMode("university")}
        >
          الجامعة كاملة
        </button>
        <button
          type="button"
          className={`btn${mode === "scoped" ? " btn--primary" : ""}`}
          disabled={!writable}
          onClick={() => setMode("scoped")}
        >
          مقيّد بكليات
        </button>
      </div>

      {mode === "scoped" && (
        <div className="field">
          <span className="field__label">الكليات المسموحة</span>
          <div className="cluster">
            {all.map((college) => (
              <button
                key={college.id}
                type="button"
                className={`btn${selected.includes(college.id) ? " btn--primary" : ""}`}
                disabled={!writable}
                onClick={() =>
                  setSelected((c) =>
                    c.includes(college.id)
                      ? c.filter((x) => x !== college.id)
                      : [...c, college.id],
                  )
                }
              >
                {college.name_ar}
              </button>
            ))}
          </div>
        </div>
      )}

      {/* The consequence, before the save. */}
      {losing.length > 0 && (
        <div className="refusal refusal--pending" style={{ marginBottom: 10 }}>
          <p className="refusal__title">أثر التضييق</p>
          <p className="refusal__body" style={{ margin: 0 }}>
            سيفقد <b>{user.full_name}</b> الوصول إلى{" "}
            <b className="num">{losing.length}</b>{" "}
            {losing.length === 2 ? "كليتين" : "كلية"} و<b className="num">{losingDepartments}</b>{" "}
            قسماً:{" "}
            {losing
              .map((id) => all.find((c) => c.id === id)?.name_ar ?? id)
              .join("، ")}
            .
          </p>
        </div>
      )}

      {changed && (
        <label className="field">
          <span className="field__label">سبب التغيير</span>
          <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
        </label>
      )}

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <Button
        variant="primary"
        disabled={!writable || !changed || (mode === "scoped" && selected.length === 0)}
        disabledReason={
          !writable
            ? writeReason
            : mode === "scoped" && selected.length === 0
              ? "نطاق مقيّد بلا كليات يمنع كل شيء"
              : !changed
                ? "لا تغيير"
                : undefined
        }
        busy={save.isPending}
        onClick={() => save.mutate()}
      >
        حفظ النطاق
      </Button>
    </Panel>
  );
}

function AccountPanel({
  user,
  writable,
  writeReason,
}: {
  user: UserDetailView;
  writable: boolean;
  writeReason: string | undefined;
}) {
  const queryClient = useQueryClient();
  const [reason, setReason] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [temporary, setTemporary] = useState<string | null>(null);

  const invalidate = () => {
    void queryClient.invalidateQueries({ queryKey: ["user", user.id] });
    void queryClient.invalidateQueries({ queryKey: ["users"] });
  };
  const onError = (error: unknown) => {
    if (isRefusal(error)) setRefusal(error);
  };

  const disable = useMutation({
    mutationFn: () => api.post(`/users/${user.id}/disable`, { reason: reason.trim() }),
    onSuccess: invalidate,
    onError,
  });

  const enable = useMutation({
    mutationFn: () => api.post(`/users/${user.id}/enable`, { reason: reason.trim() }),
    onSuccess: invalidate,
    onError,
  });

  const reset = useMutation({
    mutationFn: () =>
      api.post<{ temporary_password?: string }>(`/users/${user.id}/reset-password`, {
        reason: reason.trim(),
      }),
    onSuccess: (result) => {
      setTemporary(result.temporary_password ?? null);
      invalidate();
    },
    onError,
  });

  return (
    <Panel title="الحساب">
      <Row label="أُنشئ">
        <span className="num">{formatDateTime(user.created_at)}</span>
      </Row>
      <Row label="آخر دخول">
        <span className="num">
          {user.last_login_at ? `${formatDateTime(user.last_login_at)} (${elapsed(user.last_login_at)})` : "—"}
        </span>
      </Row>
      {user.disabled_reason && <Row label="سبب التعطيل">{user.disabled_reason}</Row>}

      <label className="field" style={{ marginTop: 10 }}>
        <span className="field__label">السبب (يُسجَّل مع أي إجراء أدناه)</span>
        <input className="input" value={reason} onChange={(e) => setReason(e.target.value)} />
      </label>

      {temporary && (
        <div className="refusal refusal--pending" style={{ marginBottom: 10 }}>
          <p className="refusal__title">كلمة مرور مؤقتة — تُعرض مرة واحدة</p>
          <p className="refusal__remedy ltr" style={{ fontFamily: "var(--mono)" }}>
            {temporary}
          </p>
        </div>
      )}

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          disabled={!writable || reason.trim().length < 3}
          disabledReason={!writable ? writeReason : "اكتب سبباً"}
          busy={reset.isPending}
          onClick={() => reset.mutate()}
        >
          إعادة تعيين كلمة المرور
        </Button>
        {user.is_active ? (
          <Button
            variant="danger"
            disabled={!writable || reason.trim().length < 3}
            disabledReason={!writable ? writeReason : "اكتب سبباً"}
            busy={disable.isPending}
            onClick={() => disable.mutate()}
          >
            تعطيل
          </Button>
        ) : (
          <Button
            disabled={!writable || reason.trim().length < 3}
            disabledReason={!writable ? writeReason : "اكتب سبباً"}
            busy={enable.isPending}
            onClick={() => enable.mutate()}
          >
            تفعيل
          </Button>
        )}
      </div>

      <p className="note" style={{ marginTop: 8 }}>
        التعطيل لا يحذف شيئاً: ما فعله المستخدم يبقى منسوباً إليه في سجل التدقيق.
      </p>
    </Panel>
  );
}
