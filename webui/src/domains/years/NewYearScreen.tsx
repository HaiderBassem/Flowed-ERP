import { useState } from "react";
import { useNavigate } from "react-router-dom";
import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import type { YearView } from "@/api/types";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Crumbs } from "@/components/Crumbs";
import { Button, Panel } from "@/components/primitives";
import { useSession } from "@/app/session";

const CODE_PATTERN = /^\d{4}-\d{4}$/;

/**
 * Defining a new academic year, in draft.
 *
 * A year's code and dates are validated again on the server (academic.NewYear)
 * — the checks here exist to catch a malformed year before it round-trips,
 * not to replace that validation.
 */
export function NewYearScreen() {
  const navigate = useNavigate();
  const queryClient = useQueryClient();
  const { can, reason } = useSession();

  const [code, setCode] = useState("");
  const [startDate, setStartDate] = useState("");
  const [endDate, setEndDate] = useState("");
  const [debtBlockPolicy, setDebtBlockPolicy] = useState("warn");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const create = useMutation({
    mutationFn: () =>
      api.post<YearView>("/academic-years", {
        code: code.trim(),
        start_date: startDate,
        end_date: endDate,
        debt_block_policy: debtBlockPolicy,
      }),
    onSuccess: (year) => {
      void queryClient.invalidateQueries({ queryKey: ["academic-years"] });
      navigate(`/years/${year.id}`);
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  const codeValid = CODE_PATTERN.test(code.trim());
  const datesValid = Boolean(startDate) && Boolean(endDate) && startDate < endDate;
  const ready = codeValid && datesValid;

  return (
    <main className="screen">
      <Crumbs items={[{ label: "السنوات الدراسية", to: "/years" }, { label: "سنة جديدة" }]} />
      <div className="screen__head">
        <h1 className="screen__title">سنة دراسية جديدة</h1>
      </div>

      <div style={{ maxWidth: 520, marginTop: 12 }}>
        <Panel title="التعريف">
          <label className="field">
            <span className="field__label">رمز السنة</span>
            <input
              className="input ltr num"
              autoFocus
              placeholder="2025-2026"
              value={code}
              onChange={(e) => setCode(e.target.value)}
            />
            {code.trim() && !codeValid && (
              <span className="field__hint">الصيغة المطلوبة: سنة-سنة، مثل 2026-2025</span>
            )}
          </label>

          <div className="cols cols--half">
            <label className="field">
              <span className="field__label">تبدأ في</span>
              <input
                className="input num"
                type="date"
                value={startDate}
                onChange={(e) => setStartDate(e.target.value)}
              />
            </label>
            <label className="field">
              <span className="field__label">تنتهي في</span>
              <input
                className="input num"
                type="date"
                value={endDate}
                onChange={(e) => setEndDate(e.target.value)}
              />
            </label>
          </div>
          {startDate && endDate && startDate >= endDate && (
            <span className="field__hint">تاريخ النهاية يجب أن يكون بعد تاريخ البداية</span>
          )}

          <label className="field">
            <span className="field__label">سياسة حجب الدين</span>
            <select
              className="input"
              value={debtBlockPolicy}
              onChange={(e) => setDebtBlockPolicy(e.target.value)}
            >
              <option value="ignore">تجاهل</option>
              <option value="warn">تحذير</option>
              <option value="block">حجب</option>
            </select>
          </label>

          {refusal && (
            <div style={{ marginBottom: 12 }}>
              <RefusalPanel refusal={refusal} />
            </div>
          )}

          <Button
            variant="primary"
            size="lg"
            disabled={!can("year.administer") || !ready}
            disabledReason={
              !can("year.administer")
                ? reason("year.administer")
                : !ready
                  ? "أكمل الحقول الإلزامية بصيغة صحيحة"
                  : undefined
            }
            busy={create.isPending}
            onClick={() => create.mutate()}
          >
            إنشاء
          </Button>

          <p className="note" style={{ marginTop: 10 }}>
            تُنشأ السنة مسودة، ولا تقبل ترحيلاً مالياً ولا تسجيلاً حتى تُفتح من كونسول السنة.
          </p>
        </Panel>
      </div>
    </main>
  );
}
