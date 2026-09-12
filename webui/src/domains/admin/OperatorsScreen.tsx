import { useState } from "react";
import { Link } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { CreateUserResponse, Role, UserDetailView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { ROLE_LABELS, useSession } from "@/app/session";
import { formatDateTime } from "@/lib/dates";

const ALL_ROLES: Role[] = [
  "cashier",
  "finance_manager",
  "registrar",
  "academic_officer",
  "auditor",
  "report_viewer",
  "admin",
];

/** Operators — §09. */
export function OperatorsScreen() {
  const { can, reason } = useSession();
  const [creating, setCreating] = useState(false);
  const [created, setCreated] = useState<CreateUserResponse | null>(null);

  const users = useQuery({
    queryKey: ["users"],
    queryFn: () => api.get<UserDetailView[]>("/users"),
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">المستخدمون</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("operators.administer")}
          disabledReason={reason("operators.administer")}
          onClick={() => setCreating((c) => !c)}
        >
          {creating ? "إغلاق" : "مستخدم جديد"}
        </Button>
      </div>

      {creating && (
        <CreateUserForm
          onCreated={(result) => {
            setCreated(result);
            setCreating(false);
          }}
          onCancel={() => setCreating(false)}
        />
      )}

      {/* Shown once and never again — the server returns it on this response
          only, and there is no route that can read it back. */}
      {created?.temporary_password && (
        <div className="refusal refusal--pending" style={{ marginTop: 12 }}>
          <p className="refusal__title">كلمة المرور المؤقتة — تُعرض مرة واحدة</p>
          <p className="refusal__body">
            سلّمها إلى <b>{created.user.full_name}</b> الآن. لا يوجد مسار يقرأها ثانيةً، ولن
            تُعرض بعد مغادرة هذه الشاشة. أول دخول سيفتح شاشة تغييرها ولا شيء غيرها.
          </p>
          <p className="refusal__remedy ltr" style={{ fontFamily: "var(--mono)", fontSize: "1.1em" }}>
            {created.temporary_password}
          </p>
          <div className="refusal__actions">
            <Button onClick={() => setCreated(null)}>سلّمتها — إخفاء</Button>
          </div>
        </div>
      )}

      <div style={{ marginTop: 12 }}>
        <Panel title="المشغّلون" flush>
          {users.isLoading && <Skeleton height={160} />}
          {users.isError &&
            (isRefusal(users.error) ? (
              <RefusalPanel refusal={users.error} onRetry={() => void users.refetch()} />
            ) : (
              <EmptyState kind="no-results" title="تعذّر جلب المستخدمين" />
            ))}
          {users.isSuccess && (users.data ?? []).length === 0 && (
            <EmptyState kind="not-yet" title="لا مستخدمين" />
          )}
          {(users.data ?? []).length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>اسم المستخدم</th>
                  <th>الاسم</th>
                  <th>الأدوار</th>
                  <th>النطاق</th>
                  <th>آخر دخول</th>
                  <th>الحالة</th>
                </tr>
              </thead>
              <tbody>
                {(users.data ?? []).map((user) => (
                  <tr key={user.id}>
                    <td className="k">
                      <Link to={`/admin/operators/${user.id}`}>{user.username}</Link>
                    </td>
                    <td>{user.full_name}</td>
                    <td>
                      {user.roles.map((role) => ROLE_LABELS[role] ?? role).join(" · ")}
                    </td>
                    <td>
                      {user.scope_mode === "scoped" ? (
                        <Chip tone="pending" hint="يرى ما داخل نطاقه فقط">
                          مقيّد
                        </Chip>
                      ) : (
                        <span className="label">الجامعة كاملة</span>
                      )}
                    </td>
                    <td className="num">{formatDateTime(user.last_login_at ?? null)}</td>
                    <td>
                      {!user.is_active ? (
                        <Chip tone="void" hint={user.disabled_reason ?? undefined}>
                          معطّل
                        </Chip>
                      ) : user.locked_until ? (
                        <Chip tone="pending">مقفل مؤقتاً</Chip>
                      ) : user.must_change_password ? (
                        <Chip tone="pending">كلمة مرور مؤقتة</Chip>
                      ) : (
                        <Chip tone="live">فعّال</Chip>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          )}
        </Panel>
      </div>
    </main>
  );
}

function CreateUserForm({
  onCreated,
  onCancel,
}: {
  onCreated: (result: CreateUserResponse) => void;
  onCancel: () => void;
}) {
  const queryClient = useQueryClient();
  const [username, setUsername] = useState("");
  const [fullName, setFullName] = useState("");
  const [roles, setRoles] = useState<Role[]>([]);
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<CreateUserResponse>("/users", {
        username: username.trim(),
        full_name: fullName.trim(),
        roles,
      }),
    onSuccess: (result) => {
      void queryClient.invalidateQueries({ queryKey: ["users"] });
      onCreated(result);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const toggle = (role: Role) =>
    setRoles((current) =>
      current.includes(role) ? current.filter((r) => r !== role) : [...current, role],
    );

  return (
    <Panel title="مستخدم جديد">
      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">اسم المستخدم</span>
          <input
            className="input ltr"
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field__label">الاسم الكامل</span>
          <input className="input" value={fullName} onChange={(e) => setFullName(e.target.value)} />
        </label>
      </div>

      <div className="field">
        <span className="field__label">الأدوار</span>
        <div className="cluster">
          {ALL_ROLES.map((role) => (
            <button
              key={role}
              type="button"
              className={`btn${roles.includes(role) ? " btn--primary" : ""}`}
              onClick={() => toggle(role)}
            >
              {ROLE_LABELS[role]}
            </button>
          ))}
        </div>
        {roles.includes("cashier") && (
          <span className="field__hint">
            الصراف يحتاج شبّاكاً عند الدخول — تسلسل الوصولات يجري لكل شبّاك.
          </span>
        )}
      </div>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!username.trim() || !fullName.trim() || roles.length === 0}
          disabledReason={roles.length === 0 ? "امنح دوراً واحداً على الأقل" : undefined}
          busy={create.isPending}
          onClick={() => create.mutate()}
        >
          إنشاء
        </Button>
        <Button variant="ghost" onClick={onCancel}>
          صرف النظر
        </Button>
        <span className="grow" />
        <span className="note">كلمة المرور تُولَّد وتُعرض مرة واحدة.</span>
      </div>
    </Panel>
  );
}

export { Row };
