import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useState,
  type ReactNode,
} from "react";

import { api, tokens } from "@/api/client";
import type { Role, TokenResponse, UserView } from "@/api/types";

/**
 * The session, and what it permits.
 *
 * Capabilities are derived from roles once, here, so that a screen asks "may
 * this operator execute a void?" rather than "is this operator a finance
 * manager?". The second form spreads the authority model across forty files
 * and gets one of them wrong.
 *
 * The interface never *enforces* any of this — the server does, on every call.
 * What this buys is §03 principle 05: a control the operator cannot use is
 * shown disabled wearing its reason instead of vanishing, because a vanished
 * control teaches them the operation does not exist.
 */

const USER_KEY = "flowed.user";

/**
 * A capability is a thing the operator can do.
 *
 * There used to be a grant table here mapping each one to the roles that held
 * it, mirroring the server's separation of duties. The system is now run from
 * a single account that holds everything, so the table said "yes" to every
 * question and the only thing it could still do was disagree with the server —
 * a control greyed out over a permission the server would have allowed.
 *
 * The names survive because the call sites read better for them: `can(
 * "payment.void.execute")` at a button says what the button does. They all
 * answer true, and the server remains the only thing that enforces anything.
 */
export type Capability =
  | "payment.record"
  | "payment.void.request"
  | "payment.void.execute"
  | "refund.request"
  | "refund.approve"
  | "refund.post"
  | "account.generate"
  | "account.adjust"
  | "student.register"
  | "student.contact"
  | "enrollment.write"
  | "enrollment.result"
  | "discount.assign"
  | "discount.confirm"
  | "config.write"
  | "year.administer"
  | "import.run"
  | "reports.read"
  | "statement.export"
  | "oversight.read"
  | "operators.administer"
  | "backup.manage"
  | "data.transfer"
  | "hosting.write"
  | "identity.write"
  | "identity.read"
  | "plan.adjust";


export interface Session {
  user: UserView | null;
  roles: Role[];
  ready: boolean;
  /** True while a stored token is being checked against /auth/me. */
  loading: boolean;
  can: (capability: Capability) => boolean;
  /** The sentence to show on a control this operator may not use. */
  reason: (capability: Capability) => string | undefined;
  is: (role: Role) => boolean;
  signIn: (credentials: { username: string; password: string }) => Promise<TokenResponse>;
  signOut: () => Promise<void>;
  refreshUser: () => Promise<void>;
}

const SessionContext = createContext<Session | null>(null);

export function SessionProvider({ children }: { children: ReactNode }) {
  const [user, setUser] = useState<UserView | null>(() => {
    const raw = localStorage.getItem(USER_KEY);
    return raw ? (JSON.parse(raw) as UserView) : null;
  });
  const [loading, setLoading] = useState(() => Boolean(tokens.access()));
  const [ready, setReady] = useState(() => !tokens.access());

  const store = useCallback((next: UserView | null) => {
    setUser(next);
    if (next) localStorage.setItem(USER_KEY, JSON.stringify(next));
    else localStorage.removeItem(USER_KEY);
  }, []);

  // A stored token is re-checked rather than trusted. Roles and scope can have
  // been changed since it was issued, and an interface drawn from a stale
  // token shows controls the server will refuse.
  useEffect(() => {
    if (!tokens.access()) return;
    let cancelled = false;
    void api
      .get<UserView>("/auth/me")
      .then((me) => {
        if (!cancelled) store(me);
      })
      .catch(() => {
        if (!cancelled) {
          tokens.clear();
          store(null);
        }
      })
      .finally(() => {
        if (!cancelled) {
          setLoading(false);
          setReady(true);
        }
      });
    return () => {
      cancelled = true;
    };
  }, [store]);

  const roles = useMemo<Role[]>(() => user?.roles ?? [], [user]);

  // Every signed-in operator may do everything; see the Capability comment.
  // Kept as a function rather than inlined at the call sites so that a future
  // second role has one place to change.
  const can = useCallback((_capability: Capability): boolean => true, []);

  const reason = useCallback((_capability: Capability): string | undefined => undefined, []);

  const signIn = useCallback(
    async (credentials: { username: string; password: string }) => {
      const result = await api.post<TokenResponse>("/auth/login", credentials, {
        anonymous: true,
      });
      tokens.set(result.access_token, result.refresh_token ?? null);
      store(result.user);
      setReady(true);
      setLoading(false);
      return result;
    },
    [store],
  );

  const signOut = useCallback(async () => {
    // Told to the server, not merely forgotten locally: sessions are rows now,
    // and a token the browser drops is a token that still works for whoever
    // else holds it.
    try {
      await api.post("/auth/logout", {});
    } catch {
      // Already gone. Clearing locally is still correct.
    }
    tokens.clear();
    store(null);
  }, [store]);

  const refreshUser = useCallback(async () => {
    const me = await api.get<UserView>("/auth/me");
    store(me);
  }, [store]);

  const value = useMemo<Session>(
    () => ({
      user,
      roles,
      ready,
      loading,
      can,
      reason,
      is: (role: Role) => roles.includes(role),
      signIn,
      signOut,
      refreshUser,
    }),
    [user, roles, ready, loading, can, reason, signIn, signOut, refreshUser],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

export function useSession(): Session {
  const session = useContext(SessionContext);
  if (!session) throw new Error("useSession outside SessionProvider");
  return session;
}

export const ROLE_LABELS: Record<Role, string> = {
  cashier: "صراف",
  finance_manager: "مدير مالي",
  registrar: "مسجّل",
  academic_officer: "موظف أكاديمي",
  auditor: "مدقق",
  report_viewer: "عارض تقارير",
  admin: "مدير إداري",
};
