import { useState } from "react";
import { Link, useSearchParams } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { HostingView } from "@/api/types";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";
import { formatDate } from "@/lib/dates";

/**
 * Hosting agreements — students studying here from another university, and
 * ours studying elsewhere.
 *
 * The field that earns its screen space is **من يقبض الرسوم**. Iraqi practice
 * genuinely varies — sometimes the home university collects, sometimes the
 * host, sometimes they split — and the domain deliberately refuses to assume
 * one answer in code, so the agreement records it per case. Whether a local
 * financial account is generated at all follows from that choice.
 */
export function HostingScreen() {
  const [params] = useSearchParams();
  const { can, reason } = useSession();
  const [registering, setRegistering] = useState(Boolean(params.get("enrollment")));

  const agreements = useQuery({
    queryKey: ["hosting"],
    queryFn: () => api.get<HostingView[]>("/hosting"),
  });

  const rows = agreements.data ?? [];

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">الاستضافة</h1>
        <span className="grow" />
        <Button
          variant="primary"
          disabled={!can("hosting.write")}
          disabledReason={reason("hosting.write")}
          onClick={() => setRegistering((r) => !r)}
        >
          {registering ? "إغلاق" : "اتفاقية جديدة"}
        </Button>
      </div>

      {registering && (
        <HostingForm
          enrollmentId={params.get("enrollment") ?? ""}
          onDone={() => setRegistering(false)}
        />
      )}

      <div style={{ marginTop: 12 }}>
        <Panel title="الاتفاقيات" flush>
          {agreements.isLoading && <Skeleton height={140} />}
          {agreements.isSuccess && rows.length === 0 && (
            <EmptyState
              kind="not-yet"
              title="لا اتفاقيات استضافة"
              detail="الاتفاقية تُسجَّل على تسجيل بعينه — من صفحة التسجيل أو من هنا بمعرّفه."
            />
          )}
          {rows.length > 0 && (
            <table className="grid">
              <thead>
                <tr>
                  <th>الاتجاه</th>
                  <th>الجامعة الأم</th>
                  <th>الجامعة المضيفة</th>
                  <th>من يقبض</th>
                  <th>حساب محلي</th>
                  <th>المدة</th>
                  <th>التسجيل</th>
                </tr>
              </thead>
              <tbody>
                {rows.map((agreement) => (
                  <tr key={agreement.id}>
                    <td>
                      {agreement.direction === "incoming" ? (
                        <Chip tone="live" hint="طالب من جامعة أخرى يدرس عندنا">
                          وافد
                        </Chip>
                      ) : (
                        <Chip tone="frozen" hint="طالبنا يدرس في جامعة أخرى">
                          موفَد
                        </Chip>
                      )}
                    </td>
                    <td>
                      {agreement.home_university ?? "—"}
                      {agreement.home_college && (
                        <span className="label"> · {agreement.home_college}</span>
                      )}
                    </td>
                    <td>
                      {agreement.host_university ?? "—"}
                      {agreement.host_college && (
                        <span className="label"> · {agreement.host_college}</span>
                      )}
                    </td>
                    <td>
                      <FeeCollector value={agreement.fee_collector} />
                    </td>
                    <td>
                      {agreement.generates_local_account ? (
                        <Chip tone="live">يتولّد</Chip>
                      ) : (
                        <Chip tone="muted" hint="الرسوم تُقبض في الجامعة الأخرى — لا حساب هنا">
                          لا يتولّد
                        </Chip>
                      )}
                    </td>
                    <td className="num">
                      {agreement.period_from ? formatDate(agreement.period_from) : "—"} →{" "}
                      {agreement.period_to ? formatDate(agreement.period_to) : "—"}
                    </td>
                    <td>
                      <Link className="k ltr" to={`/enrollments/${agreement.enrollment_id}`}>
                        فتح التسجيل
                      </Link>
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

function FeeCollector({ value }: { value: string }) {
  switch (value) {
    case "home":
      return <span>الجامعة الأم</span>;
    case "host":
      return <span>الجامعة المضيفة</span>;
    case "split":
      return <span>مناصفة بحسب الاتفاقية</span>;
    default:
      return <span className="label">{value || "—"}</span>;
  }
}

function HostingForm({
  enrollmentId: initialEnrollment,
  onDone,
}: {
  enrollmentId: string;
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [enrollmentId, setEnrollmentId] = useState(initialEnrollment);
  const [direction, setDirection] = useState<"incoming" | "outgoing">("incoming");
  const [otherUniversity, setOtherUniversity] = useState("");
  const [otherCollege, setOtherCollege] = useState("");
  const [feeCollector, setFeeCollector] = useState("");
  const [agreementRef, setAgreementRef] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const register = useMutation({
    mutationFn: () =>
      api.post<HostingView>("/hosting", {
        enrollment_id: enrollmentId.trim(),
        direction,
        // For an incoming student the *other* university is home; for an
        // outgoing one it is the host.
        ...(direction === "incoming"
          ? {
              home_university: otherUniversity.trim() || undefined,
              home_college: otherCollege.trim() || undefined,
            }
          : {
              host_university: otherUniversity.trim() || undefined,
              host_college: otherCollege.trim() || undefined,
            }),
        ...(feeCollector ? { fee_collector: feeCollector } : {}),
        ...(agreementRef.trim() ? { agreement_ref: agreementRef.trim() } : {}),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["hosting"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title="اتفاقية استضافة">
      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">معرّف التسجيل</span>
          <input
            className="input ltr"
            value={enrollmentId}
            onChange={(e) => setEnrollmentId(e.target.value)}
            placeholder="من صفحة التسجيل"
          />
          <span className="field__hint">
            الاتفاقية تخص تسجيلاً في سنة، لا الطالب مجرداً — الأسهل فتحها من صفحة التسجيل نفسها.
          </span>
        </label>
        <label className="field">
          <span className="field__label">الاتجاه</span>
          <div className="cluster">
            <button
              type="button"
              className={`btn${direction === "incoming" ? " btn--primary" : ""}`}
              onClick={() => setDirection("incoming")}
            >
              وافد إلينا
            </button>
            <button
              type="button"
              className={`btn${direction === "outgoing" ? " btn--primary" : ""}`}
              onClick={() => setDirection("outgoing")}
            >
              موفَد منا
            </button>
          </div>
        </label>
      </div>

      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">
            {direction === "incoming" ? "جامعته الأم" : "الجامعة المضيفة"}
          </span>
          <input
            className="input"
            value={otherUniversity}
            onChange={(e) => setOtherUniversity(e.target.value)}
          />
        </label>
        <label className="field">
          <span className="field__label">كليتها</span>
          <input
            className="input"
            value={otherCollege}
            onChange={(e) => setOtherCollege(e.target.value)}
          />
        </label>
      </div>

      <label className="field">
        <span className="field__label">من يقبض الرسوم — يُسجَّل لكل اتفاقية، لا افتراض في النظام</span>
        <div className="cluster">
          {(
            [
              ["home", "الجامعة الأم"],
              ["host", "الجامعة المضيفة"],
              ["split", "مناصفة"],
            ] as const
          ).map(([value, label]) => (
            <button
              key={value}
              type="button"
              className={`btn${feeCollector === value ? " btn--primary" : ""}`}
              onClick={() => setFeeCollector(value)}
            >
              {label}
            </button>
          ))}
        </div>
        <span className="field__hint">
          الممارسة العراقية تتنوع فعلاً، ولذلك يرفض النظام افتراض جواب واحد. توليد حساب محلي من
          عدمه يتبع هذا الاختيار.
        </span>
      </label>

      <label className="field">
        <span className="field__label">مرجع الاتفاقية</span>
        <input
          className="input"
          value={agreementRef}
          onChange={(e) => setAgreementRef(e.target.value)}
        />
      </label>

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!enrollmentId.trim()}
          disabledReason="سمِّ التسجيل"
          busy={register.isPending}
          onClick={() => register.mutate()}
        >
          تسجيل الاتفاقية
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
