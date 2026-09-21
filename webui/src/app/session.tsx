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

export type Capability =
  | "payment.record"
  | "payment.void.request"
  | "payment.void.execute"
  | "refund.request"
  | "refund.approve"
  | "refund.post"
  | "account.generate"
  | "account.adjust"
  | "shift.open"
  | "shift.close"
  | "shift.approve"
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
  | "sponsor.manage"
  | "sponsorship.create"
  | "settlement.read"
  | "settlement.write"
  | "hosting.write"
  | "identity.write"
  | "identity.read"
  | "plan.adjust"
  | "intent.create";

/** Who may do what, and the sentence shown when they may not. */
const GRANTS: Record<Capability, { roles: Role[]; refusal: string }> = {
  "payment.record": {
    roles: ["cashier"],
    refusal: "القبض صلاحية الصراف على شبّاك بوردية مفتوحة",
  },
  "payment.void.request": {
    roles: ["cashier", "finance_manager", "admin"],
    refusal: "طلب الإلغاء صلاحية الصراف أو المدير المالي",
  },
  "payment.void.execute": {
    roles: ["finance_manager", "admin"],
    refusal: "تنفيذ الإلغاء صلاحية المدير المالي — اطلب الإلغاء وسيصله إشعار",
  },
  "refund.request": {
    roles: ["cashier", "finance_manager", "admin"],
    refusal: "طلب الاسترجاع صلاحية الصراف أو المدير المالي",
  },
  "refund.approve": {
    roles: ["finance_manager", "admin"],
    refusal: "الموافقة على الاسترجاع صلاحية المدير المالي",
  },
  "refund.post": {
    roles: ["finance_manager", "admin"],
    refusal: "ترحيل الاسترجاع صلاحية المدير المالي",
  },
  "account.generate": {
    roles: ["finance_manager", "admin"],
    refusal: "توليد الحسابات صلاحية المدير المالي",
  },
  "account.adjust": {
    roles: ["finance_manager", "admin"],
    refusal: "قيد التسوية صلاحية المدير المالي",
  },
  "shift.open": { roles: ["cashier"], refusal: "الورديات تخص الصرافين" },
  "shift.close": { roles: ["cashier"], refusal: "الورديات تخص الصرافين" },
  "shift.approve": {
    roles: ["finance_manager", "admin"],
    // Not a permission error: it is the architecture. §09 asks it to be said
    // that way, because "you lack permission" invites a request for permission.
    refusal: "الصراف لا يعتمد درجه — الاعتماد صلاحية المدير المالي",
  },
  "student.register": {
    roles: ["registrar", "admin"],
    refusal: "تسجيل الطلبة صلاحية المسجّل",
  },
  "student.contact": {
    roles: ["registrar", "academic_officer", "admin"],
    refusal: "تعديل بيانات التواصل صلاحية المسجّل أو الموظف الأكاديمي",
  },
  "enrollment.write": {
    roles: ["registrar", "academic_officer", "admin"],
    refusal: "التسجيل في سنة صلاحية المسجّل أو الموظف الأكاديمي",
  },
  "enrollment.result": {
    roles: ["academic_officer", "admin"],
    refusal: "إدخال النتائج صلاحية الموظف الأكاديمي",
  },
  "discount.assign": {
    roles: ["registrar", "academic_officer", "finance_manager", "admin"],
    refusal: "منح الخصم صلاحية المسجّل أو المدير المالي",
  },
  "discount.confirm": {
    roles: ["finance_manager", "admin"],
    refusal: "تأكيد أهلية الخصم صلاحية المدير المالي — والمانح لا يوافق على منحه",
  },
  "config.write": {
    roles: ["finance_manager", "admin"],
    refusal: "الإعدادات المالية صلاحية المدير المالي",
  },
  "year.administer": {
    roles: ["admin", "finance_manager"],
    refusal: "إدارة السنة صلاحية المدير الإداري",
  },
  "import.run": { roles: ["registrar", "admin"], refusal: "الاستيراد صلاحية المسجّل" },
  "reports.read": {
    roles: ["finance_manager", "admin", "auditor", "report_viewer"],
    refusal: "التقارير خارج نطاق دورك",
  },
  /**
   * Wider than reports.read on purpose, and it mirrors the server exactly:
   * GET /reports/students/:id/statement admits the registrar and the cashier
   * as well, because a desk that cannot hand a student their own statement
   * sends them to another office to be told the same figures.
   */
  "statement.export": {
    roles: ["finance_manager", "admin", "auditor", "report_viewer", "registrar", "cashier"],
    refusal: "تصدير الكشف خارج نطاق دورك",
  },
  "oversight.read": {
    roles: ["finance_manager", "admin", "auditor"],
    refusal: "الرقابة صلاحية المدقق والمدير المالي",
  },
  "operators.administer": {
    roles: ["admin"],
    refusal: "إدارة المستخدمين صلاحية المدير الإداري",
  },
  "backup.manage": {
    roles: ["admin"],
    refusal: "النسخ الاحتياطي والاسترجاع صلاحية المدير الإداري",
  },
  "sponsor.manage": {
    roles: ["finance_manager", "admin"],
    refusal: "إدارة الكفلاء واعتماد اتفاقياتهم صلاحية المدير المالي",
  },
  "sponsorship.create": {
    roles: ["finance_manager", "admin", "registrar"],
    refusal: "تسجيل الكفالة صلاحية المسجّل أو المدير المالي",
  },
  "settlement.read": {
    roles: ["finance_manager", "admin", "auditor"],
    refusal: "التسويات البنكية عمل مالي يقرؤه المدقق",
  },
  "settlement.write": {
    roles: ["finance_manager", "admin"],
    refusal: "استيراد الكشوف وحسم السطور صلاحية المدير المالي — المدقق يراقب ولا يعمل",
  },
  "hosting.write": {
    roles: ["registrar", "admin"],
    refusal: "اتفاقيات الاستضافة صلاحية المسجّل",
  },
  "identity.write": {
    roles: ["registrar", "admin"],
    refusal: "تغيير الهوية القانونية صلاحية المسجّل، بقرار محكمة موثّق",
  },
  "identity.read": {
    roles: ["registrar", "admin", "auditor"],
    refusal: "سجل الهوية القانونية للمسجّل والمدقق",
  },
  "plan.adjust": {
    roles: ["finance_manager", "admin"],
    refusal: "تعديل خطة الأقساط صلاحية المدير المالي",
  },
  "intent.create": {
    roles: ["cashier", "finance_manager", "admin"],
    refusal: "بدء دفعة إلكترونية صلاحية الصراف أو المدير المالي",
  },
};

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
  signIn: (credentials: {
    username: string;
    password: string;
    cashier_desk_id?: string;
  }) => Promise<TokenResponse>;
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

  const can = useCallback(
    (capability: Capability): boolean => {
      const grant = GRANTS[capability];
      return roles.some((role) => grant.roles.includes(role));
    },
    [roles],
  );

  const reason = useCallback(
    (capability: Capability): string | undefined =>
      can(capability) ? undefined : GRANTS[capability].refusal,
    [can],
  );

  const signIn = useCallback(
    async (credentials: { username: string; password: string; cashier_desk_id?: string }) => {
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
