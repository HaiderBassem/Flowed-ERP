import { createContext, useContext, useMemo, type ReactNode } from "react";
import { useQuery } from "@tanstack/react-query";
import { useSearchParams } from "react-router-dom";

import { api } from "@/api/client";
import type { YearView } from "@/api/types";
import { useSession } from "./session";

/**
 * The working context — the three facts §03 principle 06 keeps permanently on
 * screen because each of them changes whether an operation is legitimate at
 * all: the active year and its state, the organisational scope, and the
 * drawer session.
 *
 * The active year lives in the URL rather than in memory, so a link pasted
 * into a message opens the same scene the sender was looking at (§04). The
 * failure this prevents is named in the specification: an hour of work in the
 * wrong year, which in a financial system is not discovered until
 * reconciliation.
 */

export interface WorkingContext {
  years: YearView[];
  activeYear: YearView | null;
  /** Years that still accept money — usually one, occasionally two. */
  postingYears: YearView[];
  setActiveYear: (code: string) => void;
  loading: boolean;
}

const Context = createContext<WorkingContext | null>(null);

export function WorkingContextProvider({ children }: { children: ReactNode }) {
  const { user } = useSession();
  const [params, setParams] = useSearchParams();

  // A bare array. Only the endpoints that actually paginate — student search,
  // the void and refund registers — carry the {data, total, limit, offset}
  // envelope, and reading `.data` off the others yields undefined and a year
  // selector permanently showing a dash.
  const yearsQuery = useQuery({
    queryKey: ["academic-years"],
    queryFn: () => api.get<YearView[]>("/academic-years", { query: { limit: 100 } }),
    enabled: Boolean(user),
    staleTime: 60_000,
  });

  const years = useMemo(() => yearsQuery.data ?? [], [yearsQuery.data]);

  const requestedYear = params.get("year");

  const activeYear = useMemo(() => {
    if (years.length === 0) return null;
    if (requestedYear) {
      const found = years.find((y) => y.code === requestedYear);
      if (found) return found;
    }
    // Default to the year money is actually posting into. A default of "the
    // newest row" would silently point a cashier at a draft year.
    return (
      years.find((y) => y.accepts_financial_posting) ??
      years.find((y) => y.status === "open") ??
      years[0]!
    );
  }, [years, requestedYear]);

  const value = useMemo<WorkingContext>(
    () => ({
      years,
      activeYear,
      postingYears: years.filter((y) => y.accepts_financial_posting),
      setActiveYear: (code: string) => {
        const next = new URLSearchParams(params);
        next.set("year", code);
        setParams(next, { replace: false });
      },
      loading: yearsQuery.isLoading,
    }),
    [years, activeYear, params, setParams, yearsQuery.isLoading],
  );

  return <Context.Provider value={value}>{children}</Context.Provider>;
}

export function useWorkingContext(): WorkingContext {
  const context = useContext(Context);
  if (!context) throw new Error("useWorkingContext outside provider");
  return context;
}
