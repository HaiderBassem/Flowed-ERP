import { useState } from "react";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";

import { api } from "@/api/client";
import { isRefusal, type Refusal } from "@/api/errors";
import { Chip } from "@/components/Chip";
import { RefusalPanel } from "@/components/RefusalPanel";
import { Button, EmptyState, Panel, Row, Skeleton } from "@/components/primitives";
import { formatDateTime } from "@/lib/dates";

interface ShipResult {
  blocks: number;
  entries: number;
  from_sequence: number;
  to_sequence: number;
  destination: string;
  remaining: number;
}

interface ArchiveVerify {
  ok: boolean;
  report: {
    destination: string;
    shipments: number;
    entries_checked: number;
    unshipped_entries: number;
    archive_readable: boolean;
    problems: { kind: string; sequence_no?: number; artifact?: string; detail: string }[];
  };
}

interface ChainProblem {
  sequence_no: number;
  entry_id: string;
  occurred_at: string;
  problem: string;
}

interface ChainResult {
  intact: boolean;
  problems: ChainProblem[];
}

/**
 * AuditChainViewer — §07.
 *
 * One button, and an unambiguous result: intact, or the first broken row with
 * its sequence number and its time. Not a vague "warning" — an auditor who
 * cannot tell from the screen whether the chain holds has to go and ask, and
 * the whole value of a hash chain is that nobody has to be asked.
 */
export function AuditChainScreen() {
  const chain = useQuery({
    queryKey: ["oversight", "audit", "verify"],
    queryFn: () => api.get<ChainResult>("/oversight/audit/verify"),
    // Verification walks the chain; it is not something to re-run on a timer.
    staleTime: Infinity,
    refetchOnMount: true,
  });

  return (
    <main className="screen">
      <div className="screen__head">
        <h1 className="screen__title">سلسلة التدقيق</h1>
        <span className="grow" />
        <Button variant="primary" onClick={() => void chain.refetch()} busy={chain.isFetching}>
          تحقق الآن
        </Button>
      </div>

      <div style={{ marginTop: 12 }}>
        {chain.isLoading && <Skeleton height={140} />}

        {chain.isError &&
          (isRefusal(chain.error) ? (
            <RefusalPanel refusal={chain.error} onRetry={() => void chain.refetch()} />
          ) : (
            <EmptyState kind="no-results" title="تعذّر التحقق" />
          ))}

        {chain.data?.intact && (
          <Panel>
            <EmptyState
              kind="clean"
              title="السلسلة سليمة"
              detail={
                <span className="note">
                  كل قيد يشير إلى سلفه بتجزئة صحيحة، من أول صف إلى آخره. لا صف مُدرَج ولا محذوف
                  ولا معدَّل.
                </span>
              }
            />
          </Panel>
        )}

        {chain.data && !chain.data.intact && (
          <>
            {/* An invariant breach is not softened. §10: the system caught
                itself, and hiding that is the one response that costs more
                than the breach. */}
            <div className="refusal refusal--invariant" style={{ marginBottom: 12 }}>
              <p className="refusal__title">السلسلة مكسورة</p>
              <p className="refusal__body" style={{ margin: 0 }}>
                أول صف مكسور برقمه وزمنه أدناه. أبلغ الدعم بهذه الأرقام ولا تُغلق أي سنة قبل
                حسمها.
              </p>
            </div>

            <div className="table-wrap">
              <table className="grid">
                <thead>
                  <tr>
                    <th className="n">التسلسل</th>
                    <th>القيد</th>
                    <th>الزمن</th>
                    <th>العطل</th>
                  </tr>
                </thead>
                <tbody>
                  {chain.data.problems.map((problem) => (
                    <tr key={problem.entry_id}>
                      <td className="n">{problem.sequence_no}</td>
                      <td className="k ltr">{problem.entry_id}</td>
                      <td className="num">{formatDateTime(problem.occurred_at)}</td>
                      <td>{problem.problem}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          </>
        )}
      </div>

      <ArchivePanel />
    </main>
  );
}

/**
 * The off-host archive.
 *
 * The hash chain detects an altered entry and cannot detect a deleted one:
 * verification walks what is present, so a removed tail leaves an intact
 * chain behind it. The archive comparison below is the only check in the
 * system that can see a deletion — which is why it sits on this screen, next
 * to the chain it completes.
 */
function ArchivePanel() {
  const queryClient = useQueryClient();
  const [refusal, setRefusal] = useState<Refusal | null>(null);
  const [shipped, setShipped] = useState<ShipResult | null>(null);

  const verify = useQuery({
    queryKey: ["audit", "archive", "verify"],
    queryFn: () => api.get<ArchiveVerify>("/audit/archive/verify"),
    retry: false,
    staleTime: Infinity,
  });

  const ship = useMutation({
    mutationFn: () => api.post<ShipResult>("/audit/archive/ship", {}),
    onSuccess: (result) => {
      setShipped(result);
      setRefusal(null);
      void queryClient.invalidateQueries({ queryKey: ["audit", "archive"] });
    },
    onError: (error) => {
      if (isRefusal(error)) setRefusal(error);
    },
  });

  // A deployment with no archive configured refuses with a remedy naming the
  // setting; the panel shows that refusal rather than pretending the check ran.
  const notConfigured =
    verify.isError && isRefusal(verify.error) && verify.error.code === "audit.archive_not_configured";

  return (
    <div style={{ marginTop: 14 }}>
      <Panel
        title="الأرشيف خارج المضيف"
        aside={
          <Button
            size="sm"
            variant="primary"
            busy={ship.isPending}
            disabled={notConfigured}
            disabledReason={notConfigured ? "لا أرشيف مهيأ لهذا النشر" : undefined}
            onClick={() => ship.mutate()}
          >
            شحن الآن
          </Button>
        }
      >
        <p className="note" style={{ marginBottom: 10 }}>
          السلسلة تكشف قيداً عُدِّل ولا تكشف قيداً حُذف: التحقق يمشي على الموجود، فذيلٌ
          محذوف يترك خلفه سلسلة سليمة. مقارنة الأرشيف هي الفحص الوحيد الذي يرى الحذف.
        </p>

        {verify.isLoading && <Skeleton height={80} />}

        {notConfigured && (
          <RefusalPanel refusal={verify.error as Refusal} />
        )}
        {verify.isError && !notConfigured && isRefusal(verify.error) && (
          <RefusalPanel refusal={verify.error} onRetry={() => void verify.refetch()} />
        )}

        {verify.data && (
          <>
            <Row label="الوجهة">
              <span className="k ltr">{verify.data.report.destination}</span>
            </Row>
            <Row label="شحنات">
              <span className="num">{verify.data.report.shipments}</span>
            </Row>
            <Row label="قيود قورنت">
              <span className="num">{verify.data.report.entries_checked}</span>
            </Row>
            <Row label="لم تُشحن بعد">
              <span className="num">{verify.data.report.unshipped_entries}</span>
            </Row>
            <Row label="النتيجة">
              {verify.data.ok ? (
                <Chip tone="live">الأرشيف يطابق القاعدة</Chip>
              ) : (
                <Chip tone="void">مشاكل — أدناه</Chip>
              )}
            </Row>

            {!verify.data.ok && (
              <table className="grid" style={{ marginTop: 8 }}>
                <thead>
                  <tr>
                    <th>النوع</th>
                    <th className="n">التسلسل</th>
                    <th>التفصيل</th>
                  </tr>
                </thead>
                <tbody>
                  {verify.data.report.problems.map((problem, index) => (
                    <tr key={index}>
                      <td className="k ltr">{problem.kind}</td>
                      <td className="n">{problem.sequence_no ?? "—"}</td>
                      <td>{problem.detail}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        )}

        {refusal && (
          <div style={{ marginTop: 10 }}>
            <RefusalPanel refusal={refusal} />
          </div>
        )}
        {shipped && (
          <div className="callout" style={{ marginTop: 10 }}>
            شُحنت <b className="num">{shipped.entries}</b> قيداً في{" "}
            <b className="num">{shipped.blocks}</b> كتلة حتى التسلسل{" "}
            <b className="num">{shipped.to_sequence}</b>
            {shipped.remaining > 0 ? (
              <>
                {" "}
                — بقي <b className="num">{shipped.remaining}</b>.
              </>
            ) : (
              " — لا شيء متبقٍ."
            )}
          </div>
        )}
      </Panel>
    </div>
  );
}
