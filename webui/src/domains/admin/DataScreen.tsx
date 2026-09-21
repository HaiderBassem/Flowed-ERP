import { useRef, useState } from "react";
import { useMutation } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, Panel, Row } from "@/components/primitives";
import { useSession } from "@/app/session";

/**
 * Export and import the whole system's data as CSV.
 *
 * Two buttons, and they are not symmetric. Export is safe and can be pressed
 * at any time; import replaces everything and is therefore behind a typed
 * confirmation. Putting them on one screen is deliberate — the file the import
 * accepts is exactly the file the export produced, and a screen showing only
 * one half would leave the operator guessing what the other one wanted.
 *
 * This is not the backup screen and says so. A pg_dump rebuilds a broken
 * database; these are rows in a format a person can open, read, and hand to
 * somebody who does not have PostgreSQL.
 */
export function DataScreen() {
  const { can, reason } = useSession();
  const fileRef = useRef<HTMLInputElement>(null);
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [pending, setPending] = useState<File | null>(null);
  const [confirmation, setConfirmation] = useState("");

  const exportAll = useMutation({
    mutationFn: () => api.download("/data/export"),
    onError: (error) => setRefusal(isRefusal(error) ? error : null),
  });

  const importAll = useMutation({
    mutationFn: (file: File) => {
      const form = new FormData();
      form.append("file", file);
      return api.post<ImportResult>("/data/import", form);
    },
    onSuccess: () => {
      // Everything cached describes a database that no longer exists.
      // Invalidating one query key at a time would leave the rest of the
      // interface showing the old system until each happened to refetch.
      window.location.reload();
    },
    onError: (error) => setRefusal(isRefusal(error) ? error : null),
  });

  const armed = confirmation.trim() === CONFIRM_WORD;

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">تصدير واستيراد بيانات النظام</h1>
      </div>

      {refusal && <RefusalPanel refusal={refusal} onRetry={() => setRefusal(null)} />}

      <Panel title="تصدير">
        <p className="note">
          ملف مضغوط فيه جدول CSV لكل جدول في النظام، مع ملف
          <span className="ltr"> manifest.json </span>
          يذكر إصدار المخطط وعدد الصفوف. يُفتح في Excel مباشرة.
        </p>
        <Row>
          <span className="grow" />
          <Button
            variant="primary"
            busy={exportAll.isPending}
            disabled={!can("data.transfer")}
            disabledReason={reason("data.transfer")}
            onClick={() => exportAll.mutate()}
          >
            تصدير كل البيانات
          </Button>
        </Row>
      </Panel>

      <Panel title="استيراد">
        <div className="callout callout--warn">
          <b>الاستيراد يستبدل كل البيانات الحالية</b>
          <p style={{ margin: "4px 0 0" }}>
            كل الطلبة والحسابات والدفعات الموجودة الآن تُحذف ويحلّ محلها ما في الملف. لا
            يُدمج، ولا يمكن التراجع عنه إلا باستيراد ملف تصدير أقدم. خُذ تصديراً قبل أن تبدأ.
          </p>
        </div>

        <input
          ref={fileRef}
          type="file"
          accept=".zip"
          style={{ display: "none" }}
          onChange={(event) => {
            const file = event.target.files?.[0];
            if (file) {
              setPending(file);
              setRefusal(null);
            }
            event.target.value = "";
          }}
        />

        <Row>
          <Button onClick={() => fileRef.current?.click()}>اختيار ملف…</Button>
          <span className="grow" />
          {pending && <span className="ltr">{pending.name}</span>}
        </Row>

        {pending && (
          <>
            <label className="field" style={{ marginTop: 10 }}>
              <span className="field__label">
                اكتب <b className="ltr">{CONFIRM_WORD}</b> للتأكيد
              </span>
              <input
                className="input ltr"
                value={confirmation}
                onChange={(event) => setConfirmation(event.target.value)}
                placeholder={CONFIRM_WORD}
              />
            </label>
            <Row>
              <Button
                onClick={() => {
                  setPending(null);
                  setConfirmation("");
                }}
              >
                إلغاء
              </Button>
              <span className="grow" />
              <Button
                variant="danger"
                busy={importAll.isPending}
                disabled={!armed || !can("data.transfer")}
                disabledReason={
                  !can("data.transfer")
                    ? reason("data.transfer")
                    : !armed
                      ? `اكتب ${CONFIRM_WORD} أولاً`
                      : undefined
                }
                onClick={() => pending && importAll.mutate(pending)}
              >
                استبدال كل البيانات
              </Button>
            </Row>
          </>
        )}
      </Panel>
    </main>
  );
}

/**
 * Typed rather than clicked.
 *
 * A second "are you sure" dialog is clicked through by muscle memory; a word
 * that has to be typed cannot be. Left in Latin script on purpose — it is typed
 * on a keyboard that may be in either layout, and an Arabic word would have to
 * be switched to.
 */
const CONFIRM_WORD = "REPLACE";

interface ImportResult {
  schema_version: number;
  total_rows: number;
  tables: { name: string; rows: number }[];
}
