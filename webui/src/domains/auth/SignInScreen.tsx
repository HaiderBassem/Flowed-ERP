import { useState, type FormEvent } from "react";
import { useQuery } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, Refusal } from "@/api/errors";
import type { CashierDeskView } from "@/api/types";
import logoAbstract from "@/assets/logo-mark.svg";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button } from "@/components/primitives";
import { useSession } from "@/app/session";

/**
 * Sign-in stands alone, with no navigation to a screen that would refuse.
 *
 * The desk selection is here rather than later because receipt series run per
 * year *per desk*: a cashier without one cannot post cash at all, and
 * discovering that at the moment of the first payment — with a student
 * waiting — is the wrong moment.
 */
export function SignInScreen() {
  const { signIn } = useSession();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [desk, setDesk] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);

  // The public desk list. A cashier cannot sign in without naming their desk —
  // receipt series run per desk — so this has to be readable before anyone
  // holds a token, which is why it is mounted outside authentication.
  //
  // If a deployment refuses it, the select is replaced below by an explanation
  // rather than silently disappearing: a cashier facing a form with no desk
  // field has no way to know why their sign-in keeps being refused.
  const desks = useQuery({
    queryKey: ["cashier-desks", "public"],
    queryFn: () => api.get<CashierDeskView[]>("/api/v1/public/cashier-desks", { anonymous: true }),
    retry: false,
  });

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setRefusal(null);
    setBusy(true);
    try {
      await signIn({
        username: username.trim(),
        password,
        ...(desk ? { cashier_desk_id: desk } : {}),
      });
    } catch (error) {
      setRefusal(isRefusal(error) ? error : new Refusal({
        status: 0,
        code: "unknown",
        kind: "internal",
        message: String(error),
      }));
    } finally {
      setBusy(false);
    }
  };

  const available = (desks.data ?? []).filter((d) => d.is_active);

  return (
    <div className="login">
      <form className="login__card" onSubmit={submit}>
        <img className="login__mark" src={logoAbstract} alt="" />
        <h1 className="login__brand">Flowed</h1>
        <p className="login__sub">نظام أجور وأقساط الطلبة</p>
        {/* The identity's safest tagline — the dinar-corrected line stays in
            docs until someone who writes Arabic natively sets it. */}
        <p className="login__tagline">الدفتر لا يكذب</p>

        <label className="field">
          <span className="field__label">اسم المستخدم</span>
          <input
            className="input"
            name="username"
            autoComplete="username"
            required
            autoFocus
            value={username}
            onChange={(e) => setUsername(e.target.value)}
          />
        </label>

        <label className="field">
          <span className="field__label">كلمة المرور</span>
          <input
            className="input"
            type="password"
            name="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
          />
        </label>

        {available.length > 0 ? (
          <label className="field">
            <span className="field__label">الشبّاك (للصرافين)</span>
            <select className="input" value={desk} onChange={(e) => setDesk(e.target.value)}>
              <option value="">— بدون شبّاك —</option>
              {available.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.code} — {d.name_ar}
                </option>
              ))}
            </select>
            <span className="field__hint">
              تسلسل الوصولات يجري لكل شبّاك على حدة، فالصراف يسجّل دخوله على شبّاكه.
            </span>
          </label>
        ) : (
          desks.isError && (
            <p className="field__hint" style={{ marginBottom: 12 }}>
              تعذّر جلب قائمة الشبابيك. الصراف لا يستطيع الدخول بلا شبّاك — راجع المدير الإداري.
            </p>
          )
        )}

        {refusal && (
          <div style={{ marginBottom: 12 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}

        <Button type="submit" variant="primary" size="lg" busy={busy}>
          {busy ? "…" : "دخول"}
        </Button>
      </form>
    </div>
  );
}
