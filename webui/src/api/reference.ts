import { useQuery } from "@tanstack/react-query";

import { api } from "./client";
import type { CollegeView, DepartmentView, PaymentMethodView, StudyTypeView } from "./types";

/**
 * Reference data.
 *
 * Readable by anyone signed in, changes rarely, and is needed by half the
 * screens — so it is cached long and fetched once rather than per screen. All
 * of these return bare arrays: only endpoints that genuinely paginate carry
 * the {data, total, …} envelope.
 */

const LONG = 10 * 60 * 1000;

export function useColleges() {
  return useQuery({
    queryKey: ["colleges"],
    queryFn: () => api.get<CollegeView[]>("/colleges"),
    staleTime: LONG,
  });
}

export function useDepartments() {
  return useQuery({
    queryKey: ["departments"],
    queryFn: () => api.get<DepartmentView[]>("/departments"),
    staleTime: LONG,
  });
}

export function useStudyTypes() {
  return useQuery({
    queryKey: ["study-types"],
    queryFn: () => api.get<StudyTypeView[]>("/study-types"),
    staleTime: LONG,
  });
}

export function usePaymentMethods() {
  return useQuery({
    queryKey: ["payment-methods"],
    queryFn: () => api.get<PaymentMethodView[]>("/payment-methods"),
    staleTime: LONG,
  });
}

/**
 * The student categories fee policy resolves against.
 *
 * There is no endpoint for these — the master-data service can list them but
 * no route exposes it — so the four the schema seeds are named here. Being a
 * closed set in the database, a free-text field would let a clerk invent a
 * fifth and get a resolution failure they cannot interpret.
 */
export const STUDENT_CATEGORIES = [
  { code: "REGULAR", name: "اعتيادي" },
  { code: "REPEAT", name: "معيد" },
  { code: "TRANSFER", name: "منتقل" },
  { code: "HOSTED", name: "مستضاف" },
] as const;

/**
 * Departments of one college, for the dependent select every form needs.
 *
 * No is_active filter: the reference routes already return active rows only,
 * and the field is absent from their payload — filtering on it here would
 * empty every select.
 */
export function departmentsOf(
  departments: DepartmentView[] | undefined,
  collegeId: string | undefined,
): DepartmentView[] {
  if (!departments || !collegeId) return [];
  return departments.filter((d) => d.college_id === collegeId);
}
