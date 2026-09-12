import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { SessionView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { formatDateTime } from "@/lib/dates";

/**
 * The operator's own sign-ins.
 *
 * Sessions are rows in the database, not browser state, so they can be seen
 * and ended from here. The one-button remedy — sign out everything except
 * this window — exists because the usual reason to look at this list is the
 * suspicion that a sign-in on it is not yours.
 */
export function MySessionsScreen() {
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const sessions = useQuery({
    queryKey: ["my-sessions"],
    queryFn: () => api.get<SessionView[]>("/auth/sessions"),
  });

  const revokeOthers = useMutation({
    mutationFn: () => api.post("/auth/sessions/revoke-others", {}),
    onSuccess: () => {
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["my-sessions"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const rows = sessions.data ?? [];
  const others = rows.filter((s) => !s.current && !s.revoked_at);

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">جلساتي</h1>
        <span className="grow" />
        <Button
          variant="danger"
          disabled={others.length === 0}
          disabledReason="لا جلسات أخرى مفتوحة"
          busy={revokeOthers.isPending}
          onClick={() => revokeOthers.mutate()}
        >
          إنهاء كل الجلسات عدا هذه
        </Button>
      </div>

      {refusal && (
        <div style={{ marginBottom: 12 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <Panel title="سجل الدخول" flush>
        {sessions.isLoading && <Skeleton height={140} />}
        {sessions.isSuccess && rows.length === 0 && (
          <EmptyState kind="not-yet" title="لا جلسات" />
        )}
        {rows.length > 0 && (
          <table className="grid">
            <thead>
              <tr>
                <th>أُصدرت</th>
                <th>آخر نشاط</th>
                <th>تنتهي</th>
                <th>العنوان</th>
                <th>المتصفح</th>
                <th>الحالة</th>
                <th />
              </tr>
            </thead>
            <tbody>
              {rows.map((session) => (
                <SessionRow key={session.id} session={session} />
              ))}
            </tbody>
          </table>
        )}
      </Panel>

      <p className="note" style={{ marginTop: 10, maxWidth: "72ch" }}>
        الخروج يُبلَّغ للخادم لا يُنسى محلياً فقط: رمز يهمله المتصفح يبقى صالحاً بيد من سواه.
        جلسة لا تعرفها هنا سببٌ لتغيير كلمة المرور، وتغييرها يُنهي الجلسات الأخرى افتراضاً.
      </p>
    </main>
  );
}

function SessionRow({ session }: { session: SessionView }) {
  const queryClient = useQueryClient();
  const revoke = useMutation({
    mutationFn: () => api.post(`/sessions/${session.id}/revoke`, {}),
    onSuccess: () => void queryClient.invalidateQueries({ queryKey: ["my-sessions"] }),
  });

  return (
    <tr>
      <td className="num">{formatDateTime(session.issued_at)}</td>
      <td className="num">{formatDateTime(session.last_seen_at)}</td>
      <td className="num">{formatDateTime(session.expires_at)}</td>
      <td className="num">{session.ip_address ?? "—"}</td>
      <td className="label" title={session.user_agent ?? undefined}>
        {shortAgent(session.user_agent)}
      </td>
      <td>
        {session.current ? (
          <Chip tone="live">هذه الجلسة</Chip>
        ) : session.revoked_at ? (
          <Chip tone="muted" hint={session.revoked_reason ?? undefined}>
            منتهية
          </Chip>
        ) : (
          <Chip tone="pending">مفتوحة</Chip>
        )}
      </td>
      <td>
        {!session.current && !session.revoked_at && (
          <Button size="sm" variant="danger" busy={revoke.isPending} onClick={() => revoke.mutate()}>
            إنهاء
          </Button>
        )}
      </td>
    </tr>
  );
}

function shortAgent(agent: string | null | undefined): string {
  if (!agent) return "—";
  if (agent.includes("Chrome")) return "Chrome";
  if (agent.includes("Firefox")) return "Firefox";
  if (agent.includes("Safari")) return "Safari";
  return agent.slice(0, 24);
}
