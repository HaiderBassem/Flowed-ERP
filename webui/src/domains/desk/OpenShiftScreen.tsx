import { useEffect, useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { CashierDeskView, CashierSessionView } from "@/api/types";
import { MoneyField } from "@/components/Money";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Crumbs } from "@/components/Crumbs";
import { Button, Panel } from "@/components/primitives";
import { useWorkingContext } from "@/app/working-context";
import { parseInput } from "@/lib/money";

/**
 * Opening a shift — §09.
 *
 * Collection is impossible before it, and the screen says so rather than
 * assuming the cashier knows.
 *
 * One shift per cashier: an attempt to open a second one opens the first
 * instead of refusing with something the cashier cannot act on.
 */
export function OpenShiftScreen() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { shift } = useWorkingContext();
  const [desk, setDesk] = useState("");
  const [float, setFloat] = useState("");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  // A bare array, not a page envelope — httpx.OK writes a single resource
  // unwrapped and only list endpoints that paginate carry {data, total, …}.
  const desks = useQuery({
    queryKey: ["cashier-desks"],
    queryFn: () => api.get<CashierDeskView[]>("/cashier-desks"),
  });

  useEffect(() => {
    const available = (desks.data ?? []).filter((d) => d.is_active);
    if (!desk && available.length > 0) setDesk(available[0]!.id);
  }, [desks.data, desk]);

  // Already open: send them to it rather than let them try to open a second.
  useEffect(() => {
    if (shift && shift.status === "open") navigate("/desk/sessions", { replace: true });
  }, [shift, navigate]);

  const parsed = parseInput(float);
  const openingFloat = parsed.ok ? Number(parsed.value) : 0;

  const open = useMutation({
    mutationFn: () =>
      api.post<CashierSessionView>("/cashier-sessions", {
        cashier_desk_id: desk,
        opening_float: openingFloat,
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["cashier-session"] });
      navigate("/desk");
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const available = (desks.data ?? []).filter((d) => d.is_active);

  return (
    <main className="screen">
      <Crumbs items={[{ label: "شبّاك القبض", to: "/desk" }, { label: "فتح وردية" }]} />
      <div className="screen__head">
        <h1 className="screen__title">فتح وردية</h1>
      </div>

      <div style={{ maxWidth: 520, marginTop: 12 }}>
        <Panel title="الشبّاك والرصيد الافتتاحي">
          <p className="note" style={{ marginBottom: 12 }}>
            لا يمكن القبض قبل فتح وردية. تسلسل الوصولات يجري لكل شبّاك على حدة، ولذلك الشبّاك
            يُذكر صراحةً هنا: عدم تطابقه مع شبّاك رمزك يُرفض بدل أن يُحلّ بصمت.
          </p>

          <label className="field">
            <span className="field__label">الشبّاك</span>
            <select className="input" value={desk} onChange={(e) => setDesk(e.target.value)}>
              {available.map((d) => (
                <option key={d.id} value={d.id}>
                  {d.code} — {d.name_ar}
                </option>
              ))}
            </select>
          </label>

          <MoneyField label="الرصيد الافتتاحي في الدرج" value={float} onChange={setFloat} />

          {refusal && (
            <div style={{ marginBottom: 12 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <Button
            variant="primary"
            size="lg"
            disabled={!desk}
            busy={open.isPending}
            onClick={() => open.mutate()}
          >
            فتح الوردية
          </Button>
        </Panel>
      </div>
    </main>
  );
}
