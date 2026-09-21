import { useState, type FormEvent } from "react";

import { isRefusal, Refusal } from "@/api/errors";
import logoAbstract from "@/assets/logo-mark.svg";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button } from "@/components/primitives";
import { useSession } from "@/app/session";

/** Sign-in stands alone, with no navigation to a screen that would refuse. */
export function SignInScreen() {
  const { signIn } = useSession();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    setRefusal(null);
    setBusy(true);
    try {
      await signIn({
        username: username.trim(),
        password,
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
