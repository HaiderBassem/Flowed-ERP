import { useEffect, useRef, useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, Panel, Row, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";

/**
 * Everything about this installation that is not a student: the university's
 * name, its crest, and the operator's own account.
 *
 * All of it used to live in environment variables and a database session. A
 * name that needs a server restart to change is a name that never gets changed
 * — which is why every receipt said "الجامعة" — and an account that cannot be
 * renamed is one everybody keeps signing into as "admin".
 *
 * The letterhead comes first because it is the part that shows: a receipt is
 * the only thing here a student takes away with them.
 */
export function SettingsScreen() {
  const queryClient = useQueryClient();
  const { user, refreshUser } = useSession();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [saved, setSaved] = useState<string | null>(null);

  const settings = useQuery({
    queryKey: ["settings"],
    queryFn: () => api.get<SettingsResponse>("/settings"),
  });

  const [values, setValues] = useState<Record<string, string>>({});
  useEffect(() => {
    if (settings.data) setValues(settings.data.values);
  }, [settings.data]);

  const save = useMutation({
    mutationFn: (next: Record<string, string>) =>
      api.put<SettingsResponse>("/settings", { values: next }),
    onSuccess: (result) => {
      setValues(result.values);
      setSaved("حُفظت بيانات المؤسسة.");
      setRefusal(null);
      // The letterhead is drawn into every receipt and report, so anything
      // cached that carries it is now a page with the old name on it.
      void queryClient.invalidateQueries({ queryKey: ["settings"] });
    },
    onError: (error) => setRefusal(isRefusal(error) ? error : null),
  });

  if (settings.isLoading) {
    return (
      <main className="screen">
        <Skeleton height={28} width="30%" />
        <div style={{ height: 12 }} />
        <Skeleton height={320} />
      </main>
    );
  }

  const field = (key: string) => ({
    value: values[key] ?? "",
    onChange: (event: { target: { value: string } }) => {
      setValues((current) => ({ ...current, [key]: event.target.value }));
      setSaved(null);
    },
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">الإعدادات</h1>
      </div>

      {refusal && <RefusalPanel refusal={refusal} onRetry={() => setRefusal(null)} />}

      <Panel title="بيانات المؤسسة">
        <p className="note">
          هذه البيانات تُطبع في ترويسة كل وصل وكل تقرير. غيّرها هنا — لا تحتاج إعادة تشغيل.
        </p>

        <label className="field">
          <span className="field__label">اسم الجامعة</span>
          <input className="input" placeholder="جامعة …" {...field("university_name_ar")} />
        </label>

        <label className="field">
          <span className="field__label">الكلية</span>
          <input
            className="input"
            placeholder="كلية … (اتركه فارغاً إن لم يكن له لزوم)"
            {...field("college_name_ar")}
          />
        </label>

        <div className="cols cols--halves">
          <label className="field">
            <span className="field__label">العنوان</span>
            <input className="input" placeholder="بغداد — …" {...field("address")} />
          </label>
          <label className="field">
            <span className="field__label">الهاتف</span>
            <input className="input ltr num" placeholder="07XXXXXXXXX" {...field("phone")} />
          </label>
        </div>

        <label className="field">
          <span className="field__label">سطر أسفل الوصل</span>
          <input
            className="input"
            placeholder="يُرجى الاحتفاظ بهذا السند لمراجعة الحسابات"
            {...field("receipt_footer_ar")}
          />
          <span className="field__hint">يظهر أسفل كل وصل. اتركه فارغاً لإخفائه.</span>
        </label>

        <LogoField
          value={values.logo_data_uri ?? ""}
          onChange={(next) => {
            setValues((current) => ({ ...current, logo_data_uri: next }));
            setSaved(null);
          }}
        />

        <Row>
          {saved && <span className="note">{saved}</span>}
          <span className="grow" />
          <Button variant="primary" busy={save.isPending} onClick={() => save.mutate(values)}>
            حفظ
          </Button>
        </Row>
      </Panel>

      <AccountPanel
        fullName={user?.full_name ?? ""}
        username={user?.username ?? ""}
        onSaved={() => void refreshUser()}
        onRefusal={setRefusal}
      />
    </main>
  );
}

/**
 * The crest, uploaded and held inline.
 *
 * Read into a data URI in the browser rather than uploaded to a file store,
 * because the receipt has to print identically on a machine with no network — a
 * linked image is a blank square at exactly the counter that matters. The
 * preview is not decoration either: it is the only way to catch a crest that is
 * illegible at 46 points before it is on a thousand receipts.
 */
function LogoField({ value, onChange }: { value: string; onChange: (next: string) => void }) {
  const fileRef = useRef<HTMLInputElement>(null);

  return (
    <div className="field">
      <span className="field__label">شعار الجامعة</span>
      <Row>
        {value ? (
          <img
            src={value}
            alt="شعار الجامعة"
            style={{ width: 56, height: 56, objectFit: "contain" }}
          />
        ) : (
          <span className="note">لا يوجد شعار — تُطبع الترويسة بالاسم وحده.</span>
        )}
        <span className="grow" />
        <input
          ref={fileRef}
          type="file"
          accept="image/png,image/jpeg"
          style={{ display: "none" }}
          onChange={(event) => {
            const file = event.target.files?.[0];
            event.target.value = "";
            if (!file) return;

            const reader = new FileReader();
            reader.onload = () => onChange(String(reader.result ?? ""));
            reader.readAsDataURL(file);
          }}
        />
        <Button onClick={() => fileRef.current?.click()}>اختيار صورة…</Button>
        {value && <Button onClick={() => onChange("")}>إزالة</Button>}
      </Row>
      <span className="field__hint">
        PNG أو JPEG، بحدود ٥٠٠ كيلوبايت. يُحفظ داخل النظام نفسه ليُطبع بلا إنترنت.
      </span>
    </div>
  );
}

/** The operator's own name and sign-in name. */
function AccountPanel({
  fullName,
  username,
  onSaved,
  onRefusal,
}: {
  fullName: string;
  username: string;
  onSaved: () => void;
  onRefusal: (refusal: Refusal | null) => void;
}) {
  const [name, setName] = useState(fullName);
  const [login, setLogin] = useState(username);
  const [saved, setSaved] = useState(false);

  useEffect(() => {
    setName(fullName);
    setLogin(username);
  }, [fullName, username]);

  const save = useMutation({
    mutationFn: () => api.patch("/auth/me", { full_name: name, username: login }),
    onSuccess: () => {
      setSaved(true);
      onRefusal(null);
      onSaved();
    },
    onError: (error) => onRefusal(isRefusal(error) ? error : null),
  });

  const changed = name !== fullName || login !== username;

  return (
    <Panel title="حسابي">
      <p className="note">
        الاسم يُطبع أسفل كل وصل بعد «بواسطة»، فاكتبه كما تريده أن يظهر للطالب.
      </p>

      <div className="cols cols--halves">
        <label className="field">
          <span className="field__label">الاسم</span>
          <input
            className="input"
            value={name}
            onChange={(event) => {
              setName(event.target.value);
              setSaved(false);
            }}
          />
        </label>
        <label className="field">
          <span className="field__label">اسم المستخدم</span>
          <input
            className="input ltr"
            value={login}
            onChange={(event) => {
              setLogin(event.target.value);
              setSaved(false);
            }}
          />
          <span className="field__hint">هذا ما تكتبه عند الدخول. جلستك الحالية تبقى مفتوحة.</span>
        </label>
      </div>

      <Row>
        {saved && <span className="note">حُفظ.</span>}
        <span className="grow" />
        <a className="btn" href="/app/me/password">
          تغيير كلمة المرور
        </a>
        <Button
          variant="primary"
          busy={save.isPending}
          disabled={!changed}
          disabledReason={!changed ? "لا يوجد تغيير لحفظه" : undefined}
          onClick={() => save.mutate()}
        >
          حفظ
        </Button>
      </Row>
    </Panel>
  );
}

interface SettingsResponse {
  values: Record<string, string>;
  editable: string[];
}
