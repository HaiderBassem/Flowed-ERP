import { useState, type FormEvent } from "react";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button } from "@/components/primitives";
import { useSession } from "@/app/session";

/**
 * A temporary password is a mode, not a nag.
 *
 * §13: it opens this form and nothing else, because every other route will
 * refuse the operator until it is changed — showing them a navigable
 * interface full of refusals teaches them the system is broken.
 */
export function ChangePasswordScreen() {
  const { refreshUser, signOut } = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [keepOthers, setKeepOthers] = useState(false);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [busy, setBusy] = useState(false);

  const mismatch = confirm.length > 0 && next !== confirm;

  const submit = async (event: FormEvent) => {
    event.preventDefault();
    if (mismatch) return;
    setRefusal(null);
    setBusy(true);
    try {
      await api.post("/auth/change-password", {
        current_password: current,
        new_password: next,
        keep_other_sessions: keepOthers,
      });
      await refreshUser();
    } catch (error) {
      if (isRefusal(error)) setRefusal(error);
    } finally {
      setBusy(false);
    }
  };

  return (
    <div className="login">
      <form className="login__card" onSubmit={submit}>
        <h1 className="login__brand">تغيير كلمة المرور</h1>
        <p className="login__sub">
          كلمة المرور الحالية مؤقتة. لا يمكن استعمال بقية الشاشات قبل تغييرها.
        </p>

        <label className="field">
          <span className="field__label">كلمة المرور المؤقتة</span>
          <input
            className="input"
            type="password"
            autoComplete="current-password"
            required
            autoFocus
            value={current}
            onChange={(e) => setCurrent(e.target.value)}
          />
        </label>

        <label className="field">
          <span className="field__label">كلمة المرور الجديدة</span>
          <input
            className="input"
            type="password"
            autoComplete="new-password"
            required
            value={next}
            onChange={(e) => setNext(e.target.value)}
          />
        </label>

        <label className="field">
          <span className="field__label">تأكيد كلمة المرور الجديدة</span>
          <input
            className={`input${mismatch ? " input--invalid" : ""}`}
            type="password"
            autoComplete="new-password"
            required
            value={confirm}
            onChange={(e) => setConfirm(e.target.value)}
          />
          {mismatch && <span className="field__error">الحقلان غير متطابقين</span>}
        </label>

        <label className="field" style={{ display: "flex", gap: 8, alignItems: "flex-start" }}>
          <input
            type="checkbox"
            checked={keepOthers}
            onChange={(e) => setKeepOthers(e.target.checked)}
            style={{ marginTop: 4 }}
          />
          <span>
            <span className="field__label" style={{ marginBottom: 2 }}>
              إبقاء جلساتي الأخرى مفتوحة
            </span>
            {/* The default ends them: the usual reason for changing a password
                is suspecting somebody else has it. */}
            <span className="field__hint">
              الافتراضي إنهاؤها، لأن السبب المعتاد لتغيير كلمة المرور هو الشك بأن غيرك يعرفها.
            </span>
          </span>
        </label>

        {refusal && (
          <div style={{ marginBottom: 12 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}

        <div className="cluster">
          <Button type="submit" variant="primary" busy={busy} disabled={mismatch}>
            حفظ ومتابعة
          </Button>
          <Button variant="ghost" onClick={() => void signOut()}>
            خروج
          </Button>
        </div>
      </form>
    </div>
  );
}
