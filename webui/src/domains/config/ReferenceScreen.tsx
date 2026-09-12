import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { useColleges, useDepartments, useStudyTypes } from "@/api/reference";

import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Skeleton } from "@/components/primitives";
import { useSession } from "@/app/session";

/**
 * Reference data — colleges, departments, study types.
 *
 * Additive only. There is no delete here because there is no delete in the
 * system: a college with enrollments behind it cannot be removed without
 * orphaning history, and the deactivation flag is what retires one instead.
 */
export function ReferenceScreen() {
  const { can, reason } = useSession();
  const colleges = useColleges();
  const departments = useDepartments();
  const studyTypes = useStudyTypes();
  const [adding, setAdding] = useState<"college" | "department" | "study-type" | null>(null);

  const writable = can("operators.administer");

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">البيانات المرجعية</h1>
      </div>

      <p className="screen__lede">
        إضافة فقط. لا يوجد حذف هنا لأنه لا يوجد حذف في النظام: كلية خلفها تسجيلات لا تُزال بلا
        تيتيم تاريخها، والتعطيل هو ما يُخرجها من الاستعمال.
      </p>

      {adding && (
        <ReferenceForm
          kind={adding}
          collegeOptions={colleges.data ?? []}
          onDone={() => setAdding(null)}
        />
      )}

      <div className="cols cols--thirds" style={{ marginTop: 12 }}>
        <Panel
          title="الكليات"
          aside={
            <Button
              size="sm"
              disabled={!writable}
              disabledReason={reason("operators.administer")}
              onClick={() => setAdding("college")}
            >
              +
            </Button>
          }
        >
          {colleges.isLoading && <Skeleton height={90} />}
          {(colleges.data ?? []).length === 0 && !colleges.isLoading && (
            <EmptyState kind="not-yet" title="لا كليات" />
          )}
          {(colleges.data ?? []).map((college) => (
            <div className="row" key={college.id}>
              <span className="k">{college.code}</span>
              <span className="grow">{college.name_ar}</span>
            </div>
          ))}
        </Panel>

        <Panel
          title="الأقسام"
          aside={
            <Button
              size="sm"
              disabled={!writable}
              disabledReason={reason("operators.administer")}
              onClick={() => setAdding("department")}
            >
              +
            </Button>
          }
        >
          {departments.isLoading && <Skeleton height={90} />}
          {(departments.data ?? []).length === 0 && !departments.isLoading && (
            <EmptyState kind="not-yet" title="لا أقسام" />
          )}
          {(departments.data ?? []).map((department) => (
            <div className="row" key={department.id}>
              <span className="k">{department.code}</span>
              <span className="grow">
                {department.name_ar}
                <span className="label">
                  {" "}
                  · {colleges.data?.find((c) => c.id === department.college_id)?.name_ar ?? ""}
                </span>
              </span>
              <span className="label num" title="عدد المراحل في هذا القسم">
                {department.stage_count}
              </span>
            </div>
          ))}
        </Panel>

        <Panel
          title="أنواع الدراسة"
          aside={
            <Button
              size="sm"
              disabled={!writable}
              disabledReason={reason("operators.administer")}
              onClick={() => setAdding("study-type")}
            >
              +
            </Button>
          }
        >
          {studyTypes.isLoading && <Skeleton height={90} />}
          {(studyTypes.data ?? []).map((type) => (
            <div className="row" key={type.id}>
              <span className="k">{type.code}</span>
              <span className="grow">{type.name_ar}</span>
            </div>
          ))}
        </Panel>
      </div>

      <p className="note" style={{ marginTop: 14, maxWidth: "72ch" }}>
        عدد مراحل القسم ليس تفصيلاً: الترقية الجماعية تُكمل تسجيل الطالب عند نجاحه في المرحلة
        الأخيرة بدل ترقيته إلى مرحلة غير موجودة، وهذا الرقم هو ما تقرأه.
      </p>
    </main>
  );
}

function ReferenceForm({
  kind,
  collegeOptions,
  onDone,
}: {
  kind: "college" | "department" | "study-type";
  collegeOptions: { id: string; name_ar: string }[];
  onDone: () => void;
}) {
  const queryClient = useQueryClient();
  const [code, setCode] = useState("");
  const [name, setName] = useState("");
  const [collegeId, setCollegeId] = useState(collegeOptions[0]?.id ?? "");
  const [stageCount, setStageCount] = useState("4");
  const [refusal, setRefusal] = useState<Refusal | null>(null);

  const path =
    kind === "college" ? "/colleges" : kind === "department" ? "/departments" : "/study-types";
  const title =
    kind === "college" ? "كلية جديدة" : kind === "department" ? "قسم جديد" : "نوع دراسة جديد";

  const create = useMutation({
    mutationFn: () =>
      api.post(path, {
        code: code.trim(),
        name_ar: name.trim(),
        ...(kind === "department"
          ? { college_id: collegeId, stage_count: Number(stageCount) }
          : {}),
      }),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["colleges"] });
      void queryClient.invalidateQueries({ queryKey: ["departments"] });
      void queryClient.invalidateQueries({ queryKey: ["study-types"] });
      onDone();
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  return (
    <Panel title={title}>
      <div className="cols cols--half">
        <label className="field">
          <span className="field__label">الرمز</span>
          <input className="input ltr" value={code} onChange={(e) => setCode(e.target.value)} />
        </label>
        <label className="field">
          <span className="field__label">الاسم</span>
          <input className="input" value={name} onChange={(e) => setName(e.target.value)} />
        </label>
      </div>

      {kind === "department" && (
        <div className="cols cols--half">
          <label className="field">
            <span className="field__label">الكلية</span>
            <select
              className="input"
              value={collegeId}
              onChange={(e) => setCollegeId(e.target.value)}
            >
              {collegeOptions.map((c) => (
                <option key={c.id} value={c.id}>
                  {c.name_ar}
                </option>
              ))}
            </select>
          </label>
          <label className="field">
            <span className="field__label">عدد المراحل</span>
            <input
              className="input"
              inputMode="numeric"
              dir="ltr"
              value={stageCount}
              onChange={(e) => setStageCount(e.target.value)}
            />
            <span className="field__hint">
              الترقية تُكمل التسجيل عند النجاح في هذه المرحلة بدل اختراع مرحلة تالية.
            </span>
          </label>
        </div>
      )}

      {refusal && (
        <div style={{ marginBottom: 10 }}>
          <RefusalPanel refusal={refusal} />
        </div>
      )}

      <div className="cluster">
        <Button
          variant="primary"
          disabled={!code.trim() || !name.trim()}
          busy={create.isPending}
          onClick={() => create.mutate()}
        >
          حفظ
        </Button>
        <Button variant="ghost" onClick={onDone}>
          صرف النظر
        </Button>
      </div>
    </Panel>
  );
}
