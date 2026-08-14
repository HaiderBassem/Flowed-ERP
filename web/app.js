// The operator interface.
//
// One rule shapes every line of this file: the server decides, this displays.
// No total is computed here, no eligibility is judged here, and no refusal is
// pre-empted here. If a button would be refused, the refusal comes back with a
// code and a remedy and the operator reads it — because a client that guesses
// at the rules is a client that will eventually be wrong about them, and being
// wrong about them at a cashier's window means an argument with a student.
//
// The second rule is that a cashier can work without the mouse. The desk is
// busy, the queue is long, and a payment screen that requires pointing is a
// payment screen people work around.

import { api, session } from "./api.js";
import { fmt, parseAmount, dateOnly } from "./format.js";

// ---------------------------------------------------------------------------
// Screen registry
// ---------------------------------------------------------------------------

const screens = {};
const nav = [];

// register mounts a screen and, optionally, a navigation entry.
//
// roles lists who may see the link. It is a display decision only: the server
// refuses regardless, and hiding a link the server would allow is a worse
// failure than showing one it would refuse.
function register(path, { title, render, navLabel, roles }) {
  screens[path] = { title, render, roles };
  if (navLabel) nav.push({ path, label: navLabel, roles });
}

// ---------------------------------------------------------------------------
// Rendering helpers
// ---------------------------------------------------------------------------

const view = () => document.getElementById("view");

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [key, value] of Object.entries(attrs)) {
    if (value === undefined || value === null || value === false) continue;
    if (key === "class") node.className = value;
    else if (key === "html") node.innerHTML = value;
    else if (key.startsWith("on")) node.addEventListener(key.slice(2), value);
    else node.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child === null || child === undefined || child === false) continue;
    node.append(child instanceof Node ? child : document.createTextNode(String(child)));
  }
  return node;
}

function card(title, ...children) {
  return el("section", { class: "card" }, title ? el("h2", {}, title) : null, ...children);
}

// table renders rows with a column spec. Numeric columns are marked so the
// stylesheet can align them as digits rather than as Arabic text.
function table(columns, rows, { onRowClick, rowClass } = {}) {
  const head = el("tr", {}, ...columns.map((c) => el("th", { class: c.num ? "num" : null }, c.label)));
  const body = rows.map((row) => {
    const tr = el(
      "tr",
      { class: [rowClass ? rowClass(row) : "", onRowClick ? "clickable" : ""].filter(Boolean).join(" ") },
      ...columns.map((c) => el("td", { class: c.num ? "num" : null }, c.value(row) ?? ""))
    );
    if (onRowClick) {
      tr.addEventListener("click", () => onRowClick(row));
      tr.tabIndex = 0;
      tr.addEventListener("keydown", (e) => {
        if (e.key === "Enter") onRowClick(row);
      });
    }
    return tr;
  });
  return el("table", {}, el("thead", {}, head), el("tbody", {}, ...body));
}

function status(message, kind = "") {
  const node = document.getElementById("status");
  node.textContent = message;
  node.className = kind;
  if (message) setTimeout(() => { if (node.textContent === message) node.textContent = ""; }, 6000);
}

// showError renders what the server said, including the remedy.
//
// The remedy is the point. Every refusal in this system carries one — "collect
// the balance, or have a finance manager record a written override" — and
// dropping it leaves an operator staring at a red box with nothing to do.
function showError(err) {
  const detail = err?.details?.remedy ? ` — ${err.details.remedy}` : "";
  status(`${err?.message ?? err}${detail}`, "error");
  return el("p", { class: "error" }, `${err?.message ?? err}${detail}`);
}

// confirmAct is used only for things that cannot be undone.
async function confirmAct({ title, body, detail, reason }) {
  const dialog = document.getElementById("confirm");
  document.getElementById("confirm-title").textContent = title;
  document.getElementById("confirm-body").textContent = body;
  document.getElementById("confirm-detail").textContent = detail ?? "";
  const label = document.getElementById("confirm-reason-label");
  const input = document.getElementById("confirm-reason");
  label.hidden = !reason;
  input.value = "";

  dialog.showModal();
  const result = await new Promise((resolve) => {
    dialog.addEventListener("close", () => resolve(dialog.returnValue), { once: true });
  });
  if (result !== "confirm") return null;
  if (reason && !input.value.trim()) {
    status("السبب مطلوب", "error");
    return null;
  }
  return { reason: input.value.trim() };
}

// ---------------------------------------------------------------------------
// Dashboard
// ---------------------------------------------------------------------------

register("#/", {
  title: "الرئيسية",
  navLabel: "الرئيسية",
  async render() {
    const me = session.user();
    const roles = me?.roles ?? [];

    const tiles = [];
    if (roles.includes("cashier") || roles.includes("finance_manager")) {
      tiles.push(
        card(
          "قبض",
          el("p", { class: "muted" }, "ابحث عن الطالب برقمه أو باسمه، ثم اقبض."),
          searchBox()
        )
      );
    }

    const panels = [];
    if (roles.some((r) => ["finance_manager", "admin", "auditor"].includes(r))) {
      panels.push(await reconciliationPanel(), await settlementPanel());
    }
    if (roles.includes("cashier")) panels.push(await shiftPanel());

    view().replaceChildren(
      el("div", { class: "grid" }, ...tiles, ...panels.filter(Boolean)),
      card(
        "اختصارات",
        el("p", { class: "muted", html:
          '<kbd>/</kbd> بحث · <kbd>Alt</kbd>+<kbd>P</kbd> قبض · <kbd>Esc</kbd> رجوع' })
      )
    );
  },
});

function searchBox() {
  const input = el("input", {
    id: "quick-search",
    placeholder: "رقم الطالب أو الاسم أو الهاتف",
    autocomplete: "off",
  });
  const form = el(
    "form",
    {
      onsubmit(e) {
        e.preventDefault();
        location.hash = `#/students?q=${encodeURIComponent(input.value.trim())}`;
      },
    },
    el("div", { class: "row" }, input, el("button", { class: "primary", type: "submit" }, "بحث"))
  );
  return form;
}

// reconciliationPanel shows the number that should always be zero.
async function reconciliationPanel() {
  try {
    const drift = await api.get("/api/v1/oversight/reconciliation?limit=5");
    const rows = drift?.data ?? drift ?? [];
    const count = Array.isArray(rows) ? rows.length : 0;
    return card(
      "المطابقة",
      el("p", { class: count ? "headline owing" : "headline settled" }, String(count)),
      el("p", { class: "muted" },
        count
          ? "حسابات يختلف فيها الكاش عن الحركات — يجب معالجتها قبل إغلاق السنة."
          : "لا انحراف بين الكاش والحركات.")
    );
  } catch (err) {
    return null;
  }
}

async function settlementPanel() {
  try {
    const exceptions = await api.get("/api/v1/settlements/exceptions?limit=100");
    const count = (exceptions ?? []).length;
    return card(
      "مطابقة المصرف",
      el("p", { class: count ? "headline owing" : "headline settled" }, String(count)),
      el("p", { class: "muted" }, count ? "أسطر كشف لم تُطابق." : "لا أسطر معلّقة."),
      el("a", { href: "#/settlements" }, "فتح")
    );
  } catch {
    return null;
  }
}

async function shiftPanel() {
  try {
    const shift = await api.get("/api/v1/cashier-sessions/current");
    return card(
      "الوردية",
      el("p", { class: "headline" }, fmt.money(shift?.expected_cash ?? 0)),
      el("p", { class: "muted" }, "النقد المتوقع في الدرج"),
      el("a", { href: "#/shift" }, "تفاصيل الوردية")
    );
  } catch (err) {
    return card(
      "الوردية",
      el("p", { class: "muted" }, "لا توجد وردية مفتوحة."),
      el("a", { href: "#/shift" }, "فتح وردية")
    );
  }
}

// ---------------------------------------------------------------------------
// Students
// ---------------------------------------------------------------------------

register("#/students", {
  title: "الطلبة",
  navLabel: "الطلبة",
  async render(params) {
    const query = params.get("q") ?? "";
    const input = el("input", { value: query, placeholder: "بحث", autocomplete: "off", id: "quick-search" });
    const results = el("div", {});

    async function run() {
      results.replaceChildren(el("p", { class: "muted" }, "جارٍ البحث…"));
      try {
        const page = await api.get(`/api/v1/students?q=${encodeURIComponent(input.value.trim())}&limit=50`);
        const rows = page?.data ?? [];
        if (!rows.length) {
          results.replaceChildren(el("p", { class: "muted" }, "لا نتائج."));
          return;
        }
        results.replaceChildren(
          table(
            [
              { label: "الرقم", value: (s) => s.student_no },
              { label: "الاسم", value: (s) => s.full_name },
              { label: "الأم", value: (s) => s.mother_name },
              { label: "الهاتف", value: (s) => s.phone ?? "" },
              { label: "الحالة", value: (s) => badge(s.status) },
            ],
            rows,
            { onRowClick: (s) => (location.hash = `#/student?id=${s.id}`) }
          )
        );
      } catch (err) {
        results.replaceChildren(showError(err));
      }
    }

    const form = el("form", { onsubmit(e) { e.preventDefault(); run(); } },
      el("div", { class: "row" }, input, el("button", { class: "primary" }, "بحث")));

    view().replaceChildren(card("بحث الطلبة", form), card(null, results));
    input.focus();
    if (query) run();
  },
});

register("#/student", {
  title: "ملف الطالب",
  async render(params) {
    const id = params.get("id");
    if (!id) return view().replaceChildren(el("p", {}, "لا طالب محدد."));

    view().replaceChildren(el("p", { class: "muted" }, "جارٍ التحميل…"));
    try {
      // One call: the statement is the student's whole financial position, and
      // it is the same one the portal shows them — so the desk and the student
      // are never looking at two different answers.
      const [statement, enrollments] = await Promise.all([
        api.get(`/api/v1/portal/students/${id}/statement`),
        api.get(`/api/v1/students/${id}/enrollments`).catch(() => []),
      ]);
      renderStudent(statement, enrollments, id);
    } catch (err) {
      view().replaceChildren(showError(err));
    }
  },
});

function renderStudent(statement, enrollments, id) {
  const owing = (statement.outstanding ?? 0) > 0;

  const header = card(
    `${statement.full_name} — ${statement.student_no}`,
    el("div", { class: "grid" },
      el("div", {},
        el("div", { class: `headline ${owing ? "owing" : "settled"}` }, fmt.money(statement.outstanding)),
        el("div", { class: "muted" }, owing ? "المتبقي" : "لا يوجد متبقٍ")),
      el("div", {},
        el("div", { class: "headline" }, fmt.money(statement.total_paid)),
        el("div", { class: "muted" }, "المدفوع")),
      statement.next_due
        ? el("div", {},
            el("div", { class: "headline" }, fmt.money(statement.next_due.remaining)),
            el("div", { class: "muted" }, `القسط القادم — ${statement.next_due.due_date}`))
        : null),
    el("div", { class: "row" },
      el("a", { class: "", href: `#/pay?student=${id}` },
        el("button", { class: "primary", type: "button" }, "قبض")),
      el("button", { class: "", type: "button", onclick: () => issueVerification(id) },
        "إصدار رمز تحقق"),
      el("a", { href: `#/audit?student=${id}` }, "سجل التدقيق"))
  );

  const accounts = (statement.accounts ?? []).map((account) =>
    card(
      `السنة ${account.academic_year}`,
      el("div", { class: "row" },
        stat("الرسوم", fmt.money(account.gross)),
        stat("الخصم", fmt.money(account.discount)),
        stat("المستحق", fmt.money(account.effective_net)),
        stat("المدفوع", fmt.money(account.paid)),
        stat("المتبقي", fmt.money(account.outstanding)),
        account.credit ? stat("رصيد دائن", fmt.money(account.credit)) : null),
      account.funding ? fundingBlock(account.funding) : null,
      el("h3", {}, "الأقساط"),
      table(
        [
          { label: "#", value: (i) => i.number },
          { label: "الاستحقاق", value: (i) => i.due_date },
          { label: "المبلغ", value: (i) => fmt.money(i.amount), num: true },
          { label: "المدفوع", value: (i) => fmt.money(i.paid_amount), num: true },
          { label: "المتبقي", value: (i) => fmt.money(i.remaining), num: true },
          { label: "الحالة", value: (i) => badge(i.is_overdue ? "overdue" : i.status) },
        ],
        account.installments ?? [],
        { rowClass: (i) => (i.is_overdue ? "overdue" : "") }
      ),
      el("h3", {}, "الدفعات"),
      table(
        [
          { label: "الوصل", value: (p) => p.receipt_no ?? "—" },
          { label: "التاريخ", value: (p) => dateOnly(p.paid_at) },
          { label: "الطريقة", value: (p) => p.method },
          { label: "المبلغ", value: (p) => fmt.money(p.amount), num: true },
          { label: "المسترجع", value: (p) => (p.refunded ? fmt.money(p.refunded) : ""), num: true },
          {
            label: "",
            value: (p) =>
              el("a", { href: `/api/v1/payments/${p.payment_id}/receipt`, target: "_blank" }, "طباعة"),
          },
        ],
        account.payments ?? [],
        { rowClass: (p) => (p.status === "voided" ? "voided" : "") }
      ),
      el("div", { class: "row" },
        el("a", { href: `#/plan?account=${account.account_id}` }, "تعديل جدول الأقساط"))
    )
  );

  const registrations = card(
    "التسجيلات",
    table(
      [
        { label: "السنة", value: (e) => e.academic_year_code ?? "" },
        { label: "المرحلة", value: (e) => e.stage },
        { label: "القسم", value: (e) => e.department_name ?? "" },
        { label: "الحالة", value: (e) => badge(e.status) },
        { label: "النتيجة", value: (e) => e.result ?? "" },
        {
          label: "",
          value: (e) =>
            el("a", { href: `#/enrollment?id=${e.id}` }, "تغيير الحالة"),
        },
      ],
      Array.isArray(enrollments) ? enrollments : enrollments?.data ?? []
    )
  );

  view().replaceChildren(header, ...accounts, registrations);
}

function stat(label, value) {
  return el("div", {}, el("div", { class: "muted" }, label), el("div", { class: "amount" }, value));
}

function fundingBlock(funding) {
  return el("div", { class: "detail" },
    `الجهة الراعية: مغطّى ${fmt.money(funding.sponsor_covered)} · ` +
    `مستحق على الراعي ${fmt.money(funding.sponsor_receivable)} · ` +
    `دفع الراعي ${fmt.money(funding.sponsor_paid)} · ` +
    `دفع الطالب ${fmt.money(funding.student_paid)}`);
}

async function issueVerification(studentId) {
  try {
    const result = await api.post(`/api/v1/portal/students/${studentId}/statement/verification`, {
      valid_for_days: 30,
    });
    status(`رمز التحقق: ${result.code} — صالح حتى ${dateOnly(result.expires_at)}`, "ok");
  } catch (err) {
    showError(err);
  }
}

// ---------------------------------------------------------------------------
// Collection
// ---------------------------------------------------------------------------

register("#/pay", {
  title: "قبض",
  async render(params) {
    const studentId = params.get("student");
    if (!studentId) return view().replaceChildren(searchBox());

    const [statement, methods] = await Promise.all([
      api.get(`/api/v1/portal/students/${studentId}/statement`),
      api.get("/api/v1/payment-methods"),
    ]);

    const open = (statement.accounts ?? []).filter((a) => (a.outstanding ?? 0) > 0);
    if (!open.length) {
      return view().replaceChildren(
        card("قبض", el("p", { class: "ok" }, "لا يوجد مستحق على هذا الطالب."))
      );
    }

    const account = el("select", { name: "account_id" },
      ...open.map((a) =>
        el("option", { value: a.account_id }, `${a.academic_year} — المتبقي ${fmt.money(a.outstanding)}`)));
    const amount = el("input", { name: "amount", class: "amount", inputmode: "numeric",
      value: String(open[0].outstanding), required: true });
    const method = el("select", { name: "payment_method_id" },
      ...(methods ?? []).map((m) => el("option", { value: m.id, "data-cash": m.is_cash,
        "data-ref": m.requires_reference }, m.name_ar)));
    const reference = el("input", { name: "method_reference", autocomplete: "off" });
    const payer = el("input", { name: "payer_name", autocomplete: "off" });

    // The reference field appears only when the chosen method needs one. The
    // server enforces it; this saves the cashier a refused submission.
    const referenceLabel = el("label", {}, "إشارة الحوالة/الإشعار", reference);
    function syncReference() {
      const chosen = method.selectedOptions[0];
      referenceLabel.hidden = chosen?.dataset.ref !== "true";
    }
    method.addEventListener("change", syncReference);

    const form = el("form", {
      async onsubmit(e) {
        e.preventDefault();
        const button = form.querySelector("button[type=submit]");
        button.disabled = true;
        try {
          const result = await api.post("/api/v1/payments", {
            account_id: account.value,
            amount: parseAmount(amount.value),
            payment_method_id: method.value,
            method_reference: reference.value.trim() || undefined,
            payer_name: payer.value.trim() || undefined,
          }, { idempotent: true });

          // The receipt is the point of the whole screen: it opens
          // immediately, because a cashier who has to hunt for it hands the
          // student nothing.
          status(`تم القبض — وصل ${result.payment?.receipt_no ?? ""}`, "ok");
          if (result.payment?.id) {
            window.open(`/api/v1/payments/${result.payment.id}/receipt`, "_blank");
          }
          location.hash = `#/student?id=${studentId}`;
        } catch (err) {
          form.querySelector(".error")?.remove();
          form.append(showError(err));
        } finally {
          button.disabled = false;
        }
      },
    },
      el("label", {}, "الحساب", account),
      el("label", {}, "المبلغ (دينار)", amount),
      el("label", {}, "طريقة الدفع", method),
      referenceLabel,
      el("label", {}, "اسم الدافع (إن اختلف)", payer),
      el("button", { class: "primary", type: "submit" }, "قبض وطباعة الوصل")
    );

    view().replaceChildren(
      card(`قبض — ${statement.full_name} (${statement.student_no})`, form),
      card(null, el("p", { class: "muted" },
        "التخصيص تلقائي على الأقساط الأقدم استحقاقاً، والفائض يصبح رصيداً دائناً."))
    );
    syncReference();
    amount.focus();
    amount.select();
  },
});

// ---------------------------------------------------------------------------
// Enrollment status, with its financial treatment
// ---------------------------------------------------------------------------

register("#/enrollment", {
  title: "التسجيل",
  async render(params) {
    const id = params.get("id");
    const enrollment = await api.get(`/api/v1/enrollments/${id}`);

    const target = el("select", { name: "status" },
      el("option", { value: "deferred" }, "تأجيل"),
      el("option", { value: "withdrawn" }, "انسحاب"),
      el("option", { value: "dropped_out" }, "انقطاع"),
      el("option", { value: "transferred_out" }, "نقل خارج"),
      el("option", { value: "completed" }, "إكمال/تخرج"));

    // No default is offered. The API refuses a status change that ends an
    // enrollment without one, and pre-selecting an option here would be this
    // client deciding a policy question the university owns.
    const treatment = el("select", { name: "financial_treatment" },
      el("option", { value: "" }, "— اختر المعالجة المالية —"),
      el("option", { value: "keep" }, "إبقاء الالتزام كاملاً"),
      el("option", { value: "waive_unpaid" }, "إسقاط غير المدفوع"),
      el("option", { value: "waive_all" }, "إسقاط الكل (يتحول المدفوع إلى رصيد)"),
      el("option", { value: "partial" }, "احتساب مبلغ جزئي"));
    const charge = el("input", { name: "charge_instead", class: "amount", inputmode: "numeric" });
    const chargeLabel = el("label", { hidden: true }, "المبلغ المحتسب", charge);
    treatment.addEventListener("change", () => { chargeLabel.hidden = treatment.value !== "partial"; });

    const orderRef = el("input", { name: "order_ref" });
    const reason = el("input", { name: "reason" });
    const override = el("input", { name: "graduation_override_reason" });

    const form = el("form", {
      async onsubmit(e) {
        e.preventDefault();
        try {
          const result = await api.post(`/api/v1/enrollments/${id}/status`, {
            status: target.value,
            financial_treatment: treatment.value || undefined,
            charge_instead: treatment.value === "partial" ? parseAmount(charge.value) : undefined,
            order_ref: orderRef.value.trim() || undefined,
            reason: reason.value.trim() || undefined,
            graduation_override_reason: override.value.trim() || undefined,
          });
          const t = result.financial_treatment;
          status(t ? `تم — أُسقط ${fmt.money(t.waived)} ورصيد ${fmt.money(t.credit_raised)}` : "تم", "ok");
          location.hash = `#/student?id=${enrollment.student_id}`;
        } catch (err) {
          form.querySelector(".error")?.remove();
          form.append(showError(err));
        }
      },
    },
      el("label", {}, "الحالة الجديدة", target),
      el("label", {}, "المعالجة المالية", treatment),
      chargeLabel,
      el("label", {}, "مرجع الأمر الإداري", orderRef),
      el("label", {}, "السبب", reason),
      el("label", {}, "سبب تجاوز حجب التخرج (إن لزم)", override),
      el("button", { class: "primary" }, "تنفيذ"));

    view().replaceChildren(
      card("تغيير حالة التسجيل", form),
      card(null, el("p", { class: "muted" },
        "المعالجة المالية إلزامية لكل حالة تُنهي التسجيل — لا يوجد خيار افتراضي."))
    );
  },
});

// ---------------------------------------------------------------------------
// Installment plan
// ---------------------------------------------------------------------------

register("#/plan", {
  title: "جدول الأقساط",
  async render(params) {
    const accountId = params.get("account");
    const [account, revisions] = await Promise.all([
      api.get(`/api/v1/accounts/${accountId}`),
      api.get(`/api/v1/accounts/${accountId}/plan-revisions`).catch(() => []),
    ]);

    const rows = account.installments ?? [];
    const dateInputs = new Map();

    const editable = rows.filter((i) => i.status !== "paid" && i.status !== "superseded");
    const list = el("div", {},
      ...editable.map((i) => {
        const input = el("input", { type: "date", value: i.due_date });
        dateInputs.set(i.id, input);
        return el("label", {}, `القسط ${i.number} — ${fmt.money(i.remaining)}`, input);
      }));

    const reason = el("input", { required: true });
    const form = el("form", {
      async onsubmit(e) {
        e.preventDefault();
        const due_dates = {};
        for (const [id, input] of dateInputs) {
          const original = rows.find((r) => r.id === id);
          if (input.value && input.value !== original.due_date) due_dates[id] = input.value;
        }
        if (!Object.keys(due_dates).length) return status("لا تغيير في التواريخ", "warn");
        try {
          await api.post(`/api/v1/accounts/${accountId}/plan`, {
            kind: "reschedule", reason: reason.value.trim(), due_dates,
          });
          status("تم تعديل الجدول", "ok");
          screens[location.hash.split("?")[0]] && route();
        } catch (err) {
          form.querySelector(".error")?.remove();
          form.append(showError(err));
        }
      },
    }, list, el("label", {}, "السبب", reason), el("button", { class: "primary" }, "حفظ التواريخ"));

    view().replaceChildren(
      card("تعديل مواعيد الأقساط", form),
      card("سجل التعديلات",
        table(
          [
            { label: "النوع", value: (r) => r.kind },
            { label: "السبب", value: (r) => r.reason },
            { label: "قبل", value: (r) => fmt.money(r.unpaid_before), num: true },
            { label: "بعد", value: (r) => fmt.money(r.unpaid_after), num: true },
            { label: "التاريخ", value: (r) => dateOnly(r.created_at) },
          ],
          revisions ?? []
        ))
    );
  },
});

// ---------------------------------------------------------------------------
// Settlements
// ---------------------------------------------------------------------------

register("#/settlements", {
  title: "مطابقة المصرف",
  navLabel: "المطابقة",
  roles: ["finance_manager", "admin", "auditor"],
  async render() {
    const [exceptions, unconfirmed] = await Promise.all([
      api.get("/api/v1/settlements/exceptions?limit=200"),
      api.get("/api/v1/settlements/unconfirmed?limit=200").catch(() => []),
    ]);

    const upload = el("form", {
      async onsubmit(e) {
        e.preventDefault();
        const data = new FormData(e.target);
        try {
          const result = await api.upload("/api/v1/settlements/import", data);
          status(`استُورد ${result.batch.line_count} سطراً، ${result.exceptions} بحاجة لمراجعة`, "ok");
          route();
        } catch (err) {
          upload.querySelector(".error")?.remove();
          upload.append(showError(err));
        }
      },
    },
      el("div", { class: "row" },
        el("label", {}, "المصرف", el("input", { name: "source_code", required: true, placeholder: "RAFIDAIN" })),
        el("label", {}, "الملف", el("input", { name: "file", type: "file", accept: ".csv", required: true })),
        el("button", { class: "primary" }, "استيراد")));

    view().replaceChildren(
      card("استيراد كشف", upload),
      card(`أسطر لم تُطابق (${(exceptions ?? []).length})`,
        table(
          [
            { label: "المصدر", value: (l) => l.source_code },
            { label: "السطر", value: (l) => l.line_no },
            { label: "الإشارة", value: (l) => l.external_ref ?? "" },
            { label: "المبلغ", value: (l) => fmt.money(l.amount), num: true },
            { label: "الفرق", value: (l) => (l.variance ? fmt.money(l.variance) : ""), num: true },
            { label: "الحالة", value: (l) => badge(l.match_status) },
            { label: "", value: (l) => el("button", { class: "link", type: "button",
                onclick: () => resolveLine(l) }, "معالجة") },
          ],
          exceptions ?? [])),
      card(`قبض لم يؤكده المصرف (${(unconfirmed ?? []).length})`,
        table(
          [
            { label: "الوصل", value: (p) => p.receipt_no ?? "" },
            { label: "الإشارة", value: (p) => p.method_reference ?? "" },
            { label: "الطريقة", value: (p) => p.method_code },
            { label: "المبلغ", value: (p) => fmt.money(p.amount), num: true },
            { label: "التاريخ", value: (p) => dateOnly(p.paid_at) },
          ],
          unconfirmed ?? []))
    );
  },
});

async function resolveLine(line) {
  const answer = await confirmAct({
    title: "معالجة سطر الكشف",
    body: "سيُسجَّل السطر كمُستبعد مع السبب. المطابقة اليدوية تحتاج رقم الدفعة.",
    detail: `${line.external_ref ?? ""} — ${fmt.money(line.amount)}`,
    reason: true,
  });
  if (!answer) return;
  try {
    await api.post(`/api/v1/settlements/lines/${line.line_id}/resolve`, {
      status: "ignored", note: answer.reason,
    });
    status("تمت المعالجة", "ok");
    route();
  } catch (err) {
    showError(err);
  }
}

// ---------------------------------------------------------------------------
// Reports
// ---------------------------------------------------------------------------

const reportList = [
  { path: "/api/v1/reports/debt", label: "الديون", needsYear: true },
  { path: "/api/v1/reports/aging", label: "أعمار الذمم", needsYear: true },
  { path: "/api/v1/reports/installments", label: "الأقساط", needsYear: true },
  { path: "/api/v1/reports/departments", label: "ملخص الأقسام", needsYear: true },
  { path: "/api/v1/sponsors/receivables", label: "مستحقات الرعاة", needsYear: false },
];

register("#/reports", {
  title: "التقارير",
  navLabel: "التقارير",
  roles: ["finance_manager", "admin", "auditor", "report_viewer"],
  async render() {
    const years = await api.get("/api/v1/academic-years").catch(() => []);
    const year = el("select", {},
      ...(years ?? []).map((y) => el("option", { value: y.id }, `${y.code} (${y.status})`)));

    const output = el("div", {});
    const cards = reportList.map((report) =>
      el("div", { class: "row" },
        el("strong", { style: "flex:1" }, report.label),
        el("button", { type: "button", onclick: () => runReport(report, year.value, output) }, "عرض"),
        // Fetched with the Authorization header and handed to the browser as a
        // blob, rather than linked with the token in the query string. A token
        // in a URL lands in the access log, in the Referer header and in
        // whatever the operator pastes into a message.
        el("button", { type: "button", onclick: () => downloadReport(report, year.value, "csv") }, "CSV"),
        el("button", { type: "button", onclick: () => downloadReport(report, year.value, "xlsx") }, "XLSX"),
        el("button", { type: "button", onclick: () => downloadReport(report, year.value, "pdf") }, "طباعة")));

    view().replaceChildren(
      card("التقارير", el("label", {}, "السنة الدراسية", year), ...cards),
      card(null, output)
    );
  },
});

// downloadReport fetches an export and hands it to the browser.
//
// The printable variant is opened in a window and printed; the spreadsheets
// are saved. Both go through fetch so the credential travels in a header.
async function downloadReport(report, yearID, format) {
  const params = new URLSearchParams({ format });
  if (report.needsYear && yearID) params.set("academic_year_id", yearID);

  status("جارٍ تحضير الملف…");
  try {
    const blob = await api.download(`${report.path}?${params}`);
    const url = URL.createObjectURL(blob);

    if (format === "pdf") {
      const printer = window.open(url, "_blank");
      // Printing is left to the operator: a page that printed itself on open
      // is a page that prints twice when somebody refreshes.
      if (printer) printer.addEventListener("load", () => status("جاهز للطباعة", "ok"));
    } else {
      const link = el("a", { href: url, download: `${report.label}.${format}` });
      document.body.append(link);
      link.click();
      link.remove();
      status("تم التنزيل", "ok");
    }
    // Released on the next tick: revoking immediately can cancel the download
    // in some browsers before it has read the blob.
    setTimeout(() => URL.revokeObjectURL(url), 60_000);
  } catch (err) {
    showError(err);
  }
}

async function runReport(report, yearID, output) {
  output.replaceChildren(el("p", { class: "muted" }, "جارٍ التحميل…"));
  try {
    const params = new URLSearchParams();
    if (report.needsYear && yearID) params.set("academic_year_id", yearID);
    const rows = await api.get(`${report.path}?${params}`);
    const data = rows?.data ?? rows ?? [];
    if (!Array.isArray(data) || !data.length) {
      return output.replaceChildren(el("p", { class: "muted" }, "لا بيانات."));
    }
    const columns = Object.keys(data[0]).slice(0, 8).map((key) => ({
      label: key,
      value: (row) => (typeof row[key] === "number" ? fmt.money(row[key]) : String(row[key] ?? "")),
      num: typeof data[0][key] === "number",
    }));
    output.replaceChildren(table(columns, data));
  } catch (err) {
    output.replaceChildren(showError(err));
  }
}

// ---------------------------------------------------------------------------
// Users and master data
// ---------------------------------------------------------------------------

register("#/users", {
  title: "المستخدمون",
  navLabel: "المستخدمون",
  roles: ["admin", "auditor"],
  async render() {
    const users = await api.get("/api/v1/users");
    view().replaceChildren(
      card("إنشاء مستخدم", createUserForm()),
      card("المستخدمون",
        table(
          [
            { label: "المستخدم", value: (u) => u.username },
            { label: "الاسم", value: (u) => u.full_name },
            { label: "الأدوار", value: (u) => (u.roles ?? []).join("، ") },
            { label: "النطاق", value: (u) => u.scope_mode },
            { label: "الحالة", value: (u) => badge(u.is_active ? "active" : "disabled") },
            { label: "", value: (u) => el("button", { class: "link", type: "button",
                onclick: () => disableUser(u) }, u.is_active ? "تعطيل" : "تفعيل") },
          ],
          users ?? []))
    );
  },
});

function createUserForm() {
  const username = el("input", { required: true });
  const fullName = el("input", { required: true });
  const roles = el("select", { multiple: true, size: 4 },
    ...["admin", "finance_manager", "cashier", "registrar", "academic_officer", "report_viewer", "auditor"]
      .map((r) => el("option", { value: r }, r)));

  return el("form", {
    async onsubmit(e) {
      e.preventDefault();
      try {
        const result = await api.post("/api/v1/users", {
          username: username.value.trim(),
          full_name: fullName.value.trim(),
          roles: [...roles.selectedOptions].map((o) => o.value),
        });
        // Shown once and never again, which is why it is put where it cannot
        // be missed rather than in a toast that fades.
        await confirmAct({
          title: "كلمة المرور المؤقتة",
          body: "تُعرض مرة واحدة فقط. سلّمها للمستخدم — سيُطلب منه تغييرها عند أول دخول.",
          detail: result.temporary_password ?? "(تم تعيين كلمة مرور)",
        });
        route();
      } catch (err) {
        showError(err);
      }
    },
  },
    el("div", { class: "row" },
      el("label", {}, "اسم المستخدم", username),
      el("label", {}, "الاسم الكامل", fullName),
      el("label", {}, "الأدوار", roles),
      el("button", { class: "primary" }, "إنشاء")));
}

async function disableUser(user) {
  if (user.is_active) {
    const answer = await confirmAct({
      title: "تعطيل الحساب",
      body: "ستُنهى كل جلسات هذا المستخدم فوراً.",
      detail: user.username,
      reason: true,
    });
    if (!answer) return;
    try {
      await api.post(`/api/v1/users/${user.id}/disable`, { reason: answer.reason });
      status("تم التعطيل", "ok");
      route();
    } catch (err) { showError(err); }
    return;
  }
  try {
    await api.post(`/api/v1/users/${user.id}/enable`, {});
    status("تم التفعيل", "ok");
    route();
  } catch (err) { showError(err); }
}

register("#/master", {
  title: "البيانات الأساسية",
  navLabel: "البيانات الأساسية",
  roles: ["admin"],
  async render() {
    const [colleges, departments, methods, desks] = await Promise.all([
      api.get("/api/v1/colleges"),
      api.get("/api/v1/departments"),
      api.get("/api/v1/payment-methods"),
      api.get("/api/v1/cashier-desks"),
    ]);

    view().replaceChildren(
      card("الكليات", table([
        { label: "الرمز", value: (c) => c.code },
        { label: "الاسم", value: (c) => c.name_ar },
        { label: "فعّال", value: (c) => (c.is_active ? "نعم" : "لا") },
      ], colleges ?? [])),
      card("الأقسام", table([
        { label: "الرمز", value: (d) => d.code },
        { label: "الاسم", value: (d) => d.name_ar },
        { label: "السنوات", value: (d) => d.stage_count, num: true },
      ], departments ?? [])),
      card("طرق الدفع", table([
        { label: "الرمز", value: (m) => m.code },
        { label: "الاسم", value: (m) => m.name_ar },
        { label: "نقد", value: (m) => (m.is_cash ? "نعم" : "لا") },
        { label: "يتطلب إشارة", value: (m) => (m.requires_reference ? "نعم" : "لا") },
      ], methods ?? [])),
      card("الشبابيك", el("div", {},
        newDeskForm(),
        table([
          { label: "الرمز", value: (d) => d.code },
          { label: "الاسم", value: (d) => d.name_ar },
          { label: "فعّال", value: (d) => (d.is_active ? "نعم" : "لا") },
        ], desks ?? [])))
    );
  },
});

function newDeskForm() {
  const code = el("input", { required: true, placeholder: "D02" });
  const name = el("input", { required: true, placeholder: "شباك 2" });
  return el("form", {
    async onsubmit(e) {
      e.preventDefault();
      try {
        await api.post("/api/v1/cashier-desks", { code: code.value.trim(), name_ar: name.value.trim() });
        status("تم فتح الشباك", "ok");
        route();
      } catch (err) { showError(err); }
    },
  }, el("div", { class: "row" },
    el("label", {}, "الرمز", code), el("label", {}, "الاسم", name),
    el("button", { class: "primary" }, "إضافة")));
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

register("#/audit", {
  title: "سجل التدقيق",
  navLabel: "التدقيق",
  roles: ["auditor", "admin", "finance_manager"],
  async render(params) {
    const studentId = params.get("student");
    const chain = await api.get("/api/v1/oversight/audit/verify").catch(() => null);
    const problems = chain?.problems ?? chain ?? [];

    const entries = studentId
      ? await api.get(`/api/v1/students/${studentId}/audit?limit=100`).catch(() => [])
      : [];

    view().replaceChildren(
      card("سلسلة التدقيق",
        el("p", { class: problems.length ? "headline owing" : "headline settled" },
          problems.length ? `${problems.length} خلل` : "سليمة"),
        el("p", { class: "muted" },
          "كل قيد يحمل بصمة سابقه — أي تعديل لاحق يكسر التحقق ويحدد موضعه.")),
      studentId
        ? card("قيود الطالب", table([
            { label: "الوقت", value: (e) => dateOnly(e.occurred_at) },
            { label: "الإجراء", value: (e) => e.action },
            { label: "المستخدم", value: (e) => e.actor_username },
            { label: "السبب", value: (e) => e.reason ?? "" },
          ], entries ?? []))
        : null
    );
  },
});

// ---------------------------------------------------------------------------
// Shift
// ---------------------------------------------------------------------------

register("#/shift", {
  title: "الوردية",
  roles: ["cashier", "finance_manager", "admin"],
  async render() {
    let shift = null;
    try { shift = await api.get("/api/v1/cashier-sessions/current"); } catch { /* none open */ }

    if (!shift) {
      const float = el("input", { class: "amount", inputmode: "numeric", value: "0" });
      const form = el("form", {
        async onsubmit(e) {
          e.preventDefault();
          try {
            await api.post("/api/v1/cashier-sessions", { opening_float: parseAmount(float.value) });
            status("فُتحت الوردية", "ok");
            route();
          } catch (err) { showError(err); }
        },
      }, el("label", {}, "الرصيد الافتتاحي", float), el("button", { class: "primary" }, "فتح وردية"));
      return view().replaceChildren(card("فتح وردية", form));
    }

    const counted = el("input", { class: "amount", inputmode: "numeric" });
    const reason = el("input", {});
    const close = el("form", {
      async onsubmit(e) {
        e.preventDefault();
        const answer = await confirmAct({
          title: "إغلاق الوردية",
          body: "سيُقارن النقد المعدود بالمتوقع، وأي فرق يستلزم سبباً وموافقة.",
          detail: `المتوقع ${fmt.money(shift.expected_cash)}`,
        });
        if (!answer) return;
        try {
          await api.post(`/api/v1/cashier-sessions/${shift.session?.id ?? shift.id}/close`, {
            counted_cash: parseAmount(counted.value),
            variance_reason: reason.value.trim() || undefined,
          });
          status("أُغلقت الوردية", "ok");
          route();
        } catch (err) { showError(err); }
      },
    },
      el("label", {}, "النقد المعدود", counted),
      el("label", {}, "سبب الفرق (إن وجد)", reason),
      el("button", { class: "primary" }, "إغلاق"));

    view().replaceChildren(
      card("الوردية الحالية",
        el("div", { class: "row" },
          stat("الافتتاحي", fmt.money(shift.opening_float ?? 0)),
          stat("المتوقع", fmt.money(shift.expected_cash ?? 0)))),
      card("إغلاق", close)
    );
  },
});

// ---------------------------------------------------------------------------
// Router and shell
// ---------------------------------------------------------------------------

function badge(value) {
  const kind = { paid: "paid", active: "paid", pending: "open", partially_paid: "open",
    overdue: "late", unmatched: "late", duplicate: "late", variance: "late", disabled: "late" }[value] ?? "";
  return el("span", { class: `badge ${kind}` }, value ?? "");
}

async function route() {
  const [path, queryString] = location.hash.split("?");
  const screen = screens[path || "#/"] ?? screens["#/"];
  const params = new URLSearchParams(queryString ?? "");

  document.querySelectorAll("#nav a").forEach((a) => {
    a.toggleAttribute("aria-current", a.getAttribute("href") === (path || "#/"));
    if (a.getAttribute("href") === (path || "#/")) a.setAttribute("aria-current", "page");
  });

  try {
    await screen.render(params);
  } catch (err) {
    if (err?.status === 401) return showLogin();
    view().replaceChildren(showError(err));
  }
  view().focus();
}

function renderNav() {
  const roles = session.user()?.roles ?? [];
  const links = nav
    .filter((item) => !item.roles || item.roles.some((r) => roles.includes(r)))
    .map((item) => el("a", { href: item.path }, item.label));
  document.getElementById("nav").replaceChildren(...links);
  document.getElementById("whoami").textContent =
    `${session.user()?.full_name ?? ""} (${roles.join("، ")})`;
}

// mustChangePassword sends an operator holding a credential somebody else set
// straight to the form. Every other route would refuse them anyway; showing
// them the refusal instead of the fix would be a puzzle rather than a screen.
function passwordGate() {
  if (!session.user()?.must_change_password) return false;
  view().replaceChildren(card("تغيير كلمة المرور مطلوب",
    el("p", { class: "muted" }, "هذا الحساب يستخدم كلمة مرور عيّنها شخص آخر. غيّرها للمتابعة."),
    changePasswordForm()));
  return true;
}

function changePasswordForm() {
  const current = el("input", { type: "password", required: true, autocomplete: "current-password" });
  const next = el("input", { type: "password", required: true, autocomplete: "new-password" });
  return el("form", {
    async onsubmit(e) {
      e.preventDefault();
      try {
        await api.post("/api/v1/auth/change-password", {
          current_password: current.value, new_password: next.value,
        });
        const me = await api.get("/api/v1/auth/me");
        session.setUser(me);
        status("تم تغيير كلمة المرور", "ok");
        location.hash = "#/";
        boot();
      } catch (err) {
        e.target.querySelector(".error")?.remove();
        e.target.append(showError(err));
      }
    },
  },
    el("label", {}, "كلمة المرور الحالية", current),
    el("label", {}, "كلمة المرور الجديدة", next),
    el("button", { class: "primary" }, "حفظ"));
}

function showLogin() {
  document.getElementById("app").hidden = true;
  document.getElementById("login").hidden = false;
  loadDesks();
}

async function loadDesks() {
  try {
    const desks = await api.get("/api/v1/cashier-desks", { anonymous: true });
    const select = document.getElementById("desk-select");
    for (const desk of desks ?? []) {
      select.append(el("option", { value: desk.id }, `${desk.code} — ${desk.name_ar}`));
    }
  } catch {
    // The desk list needs a credential in most deployments; a cashier can
    // still type nothing and be told to pick one.
  }
}

async function boot() {
  if (!session.token()) return showLogin();

  try {
    const me = await api.get("/api/v1/auth/me");
    session.setUser(me);
  } catch (err) {
    if (err?.status === 401 || err?.status === 403) {
      // A must-change-password account can read /auth/me and nothing else, so
      // a 403 here is not necessarily a dead session.
      if (err?.code !== "auth.password_change_required") {
        session.clear();
        return showLogin();
      }
    }
  }

  document.getElementById("login").hidden = true;
  document.getElementById("app").hidden = false;
  renderNav();

  if (passwordGate()) return;
  await route();
  await refreshAlerts();
}

// refreshAlerts surfaces what an operator must not miss, wherever they are.
async function refreshAlerts() {
  const roles = session.user()?.roles ?? [];
  if (!roles.some((r) => ["finance_manager", "admin", "auditor"].includes(r))) return;

  const banner = document.getElementById("alerts");
  const items = [];
  try {
    const exceptions = await api.get("/api/v1/settlements/exceptions?limit=1");
    if ((exceptions ?? []).length) {
      items.push(el("div", { class: "alert" },
        "أسطر كشف مصرفي لم تُطابق بعد — ",
        el("a", { href: "#/settlements" }, "معالجة")));
    }
  } catch { /* nothing to say */ }

  try {
    const drift = await api.get("/api/v1/oversight/reconciliation?limit=1");
    const rows = drift?.data ?? drift ?? [];
    if (rows.length) {
      items.push(el("div", { class: "alert bad" },
        "انحراف في مطابقة الحسابات — يمنع إغلاق السنة."));
    }
  } catch { /* nothing to say */ }

  banner.replaceChildren(...items);
  banner.hidden = !items.length;
}

// ---------------------------------------------------------------------------
// Wiring
// ---------------------------------------------------------------------------

document.getElementById("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const form = new FormData(e.target);
  const error = document.getElementById("login-error");
  error.hidden = true;
  try {
    await session.login({
      username: form.get("username"),
      password: form.get("password"),
      cashier_desk_id: form.get("cashier_desk_id") || undefined,
    });
    boot();
  } catch (err) {
    error.textContent = err?.message ?? "تعذّر تسجيل الدخول";
    error.hidden = false;
  }
});

document.getElementById("logout").addEventListener("click", async () => {
  await session.logout();
  showLogin();
});

window.addEventListener("hashchange", route);

// Keyboard shortcuts. A cashier desk is busy and a screen that requires
// pointing is a screen people work around.
document.addEventListener("keydown", (e) => {
  if (e.key === "/" && !["INPUT", "SELECT", "TEXTAREA"].includes(document.activeElement.tagName)) {
    e.preventDefault();
    const search = document.getElementById("quick-search");
    if (search) search.focus();
    else location.hash = "#/students";
  }
  if (e.altKey && e.key.toLowerCase() === "p") location.hash = "#/pay";
  if (e.key === "Escape" && location.hash !== "#/") history.back();
});

document.getElementById("build").textContent = "flowed";
boot();
