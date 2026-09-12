/**
 * The refusal envelope.
 *
 * Every refusal from this API arrives with a stable machine code, a message,
 * often a `details` map carrying a remedy, and a request id. §07's refusal
 * panel and §10's fault table are both built on the assumption that all four
 * survive the trip to the screen — so nothing here flattens a refusal into
 * "request failed". The remedy is frequently the most useful sentence in the
 * system and it is the first thing a naive client loses.
 *
 * The kind is not on the wire; it is reconstructed from the status below.
 */

/** Kinds as the server's shared.Kind names them. */
export type ErrorKind =
  | "validation"
  | "not_found"
  | "conflict"
  | "precondition_failed"
  | "forbidden"
  | "unauthorized"
  | "invariant_violation"
  | "rate_limited"
  | "internal"
  | "unreachable";

export interface RefusalDetails {
  remedy?: string;
  [key: string]: unknown;
}

export class Refusal extends Error {
  readonly status: number;
  readonly code: string;
  readonly kind: ErrorKind;
  readonly details: RefusalDetails;
  readonly requestId: string | undefined;
  /** Seconds to wait, when the server said so (429). */
  readonly retryAfter: number | undefined;

  constructor(init: {
    status: number;
    code: string;
    kind: ErrorKind;
    message: string;
    details?: RefusalDetails;
    requestId?: string;
    retryAfter?: number;
  }) {
    super(init.message);
    this.name = "Refusal";
    this.status = init.status;
    this.code = init.code;
    this.kind = init.kind;
    this.details = init.details ?? {};
    this.requestId = init.requestId;
    this.retryAfter = init.retryAfter;
  }

  /** What to do instead. Handlers are asked to supply this; many do. */
  get remedy(): string | undefined {
    const remedy = this.details["remedy"];
    return typeof remedy === "string" ? remedy : undefined;
  }

  /**
   * True when the operation may have reached the server despite the failure.
   *
   * "Failed" is a lie for a dropped connection: the payment may well have
   * posted. Retrying under the same idempotency key is safe, and §02 asks the
   * interface to say so out loud rather than leave a cashier guessing in front
   * of a student.
   */
  get outcomeUnknown(): boolean {
    return this.kind === "unreachable";
  }
}

/** A network failure, framed as what it actually is rather than as "failed". */
export function unreachable(cause: unknown): Refusal {
  return new Refusal({
    status: 0,
    code: "network.unreachable",
    kind: "unreachable",
    message: "لم يصلنا جواب من الخادم",
    details: {
      remedy:
        "أعد المحاولة بنفس مفتاح العملية. إن كانت العملية قد نُفِّذت فلن تتكرر، وإن لم تُنفَّذ فستُنفَّذ الآن.",
      cause: cause instanceof Error ? cause.message : String(cause),
    },
  });
}

/**
 * The kind, derived from the status.
 *
 * The wire envelope carries code, message, details and request_id — not the
 * kind — so it is reconstructed here from httpx.StatusForKind, which this
 * mirrors. Two things about that mapping are easy to get wrong:
 *
 * A business precondition is **422**, not 412. 412 belongs to conditional
 * request headers; paying into a closed year is a well-formed request the
 * server understood and refused. Reading 412 here would send every refusal
 * panel in §10 down the validation path.
 *
 * invariant_violation and internal are both 500 and cannot be told apart from
 * the outside: the server strips the message of both, on purpose, because
 * those messages quote statements and row contents. §10 wants a louder
 * treatment for an invariant breach, so the panel keys off the code when the
 * server names one and otherwise treats a 500 as internal.
 */
function kindFromStatus(status: number, code: string): ErrorKind {
  switch (status) {
    case 400:
      return "validation";
    case 401:
      return "unauthorized";
    case 403:
      return "forbidden";
    case 404:
      return "not_found";
    case 409:
      return "conflict";
    case 422:
      return "precondition_failed";
    case 429:
      return "rate_limited";
    default:
      if (status >= 500) {
        return /invariant/i.test(code) ? "invariant_violation" : "internal";
      }
      return "validation";
  }
}

export function refusalFrom(status: number, body: unknown, headers?: Headers): Refusal {
  const envelope =
    body && typeof body === "object" && "error" in body
      ? ((body as { error: unknown }).error as Record<string, unknown>)
      : ({} as Record<string, unknown>);

  const retryAfterHeader = headers?.get("Retry-After");
  const retryAfter = retryAfterHeader ? Number(retryAfterHeader) : undefined;

  const code = typeof envelope["code"] === "string" ? envelope["code"] : `http.${status}`;

  return new Refusal({
    status,
    code,
    kind: kindFromStatus(status, code),
    message:
      typeof envelope["message"] === "string" && envelope["message"].length > 0
        ? envelope["message"]
        : defaultMessage(status),
    details: (envelope["details"] as RefusalDetails) ?? {},
    requestId:
      typeof envelope["request_id"] === "string"
        ? envelope["request_id"]
        : (headers?.get("X-Request-Id") ?? undefined),
    retryAfter: Number.isFinite(retryAfter) ? retryAfter : undefined,
  });
}

function defaultMessage(status: number): string {
  switch (status) {
    case 401:
      return "انتهت الجلسة";
    case 403:
      return "لا تملك صلاحية هذا الإجراء";
    case 404:
      return "غير موجود";
    case 429:
      return "تجاوزت حد الطلبات";
    default:
      return status >= 500 ? "عطل في الخادم" : `طلب مرفوض (${status})`;
  }
}

export const isRefusal = (e: unknown): e is Refusal => e instanceof Refusal;
