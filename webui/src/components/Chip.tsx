import type { ReactNode } from "react";

import {
  describe,
  labelStudyType,
  studyTypeChipTone,
  type Entity,
  type StateLabel,
  type Tone,
} from "@/design/lexicon";

/**
 * The one badge. Every state in the interface renders through it, so the
 * lexicon in §05 stays a lexicon rather than a suggestion.
 */
export function Chip({
  tone,
  children,
  hint,
  title,
}: {
  tone: Tone;
  children: ReactNode;
  hint?: string | undefined;
  title?: string | undefined;
}) {
  return (
    <span
      className={`chip chip--${tone}`}
      {...(hint ? { "data-hint": "", title: hint } : title ? { title } : {})}
    >
      {children}
    </span>
  );
}

/** A chip resolved from a domain status through the lexicon. */
export function StateChip({
  entity,
  status,
}: {
  entity: Entity;
  status: string | null | undefined;
}) {
  const state: StateLabel = describe(entity, status);
  return (
    <Chip tone={state.tone} hint={state.hint}>
      {state.label}
    </Chip>
  );
}

/**
 * A study type badge — Morning/Evening/Parallel, each its own colour so the
 * three are told apart at a glance in a list or search result.
 *
 * Not built on Chip/Tone: study type is a distinguishing label, not one of
 * the five lexicon states, and giving it its own tones keeps "green" meaning
 * exactly one thing (§05 in lexicon.ts).
 */
export function StudyTypeChip({ code }: { code: string | null | undefined }) {
  if (!code) return null;
  return <span className={`chip chip--${studyTypeChipTone(code)}`}>{labelStudyType(code)}</span>;
}
