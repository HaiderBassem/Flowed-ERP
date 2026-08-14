// Tests for the only logic in the client worth testing on its own.
//
// Money formatting and parsing are where a client can lose a dinar without the
// server ever knowing, so they are tested here rather than trusted.

import test from "node:test";
import assert from "node:assert/strict";
import { money, parseAmount, dateOnly } from "../format.js";

test("money groups thousands and never shows a fraction", () => {
  assert.equal(money(1500000), "1,500,000");
  assert.equal(money(0), "0");
  assert.equal(money(500), "500");
  // IQD has no subunit; a decimal on the screen invites somebody to type one
  // into a field the API refuses.
  assert.equal(money(1234567.89), "1,234,567");
  assert.equal(money(-250000), "-250,000");
});

test("money survives absent and unreadable values", () => {
  assert.equal(money(null), "0");
  assert.equal(money(undefined), "0");
  assert.equal(money("not a number"), "0");
});

test("parseAmount accepts what an operator actually types", () => {
  assert.equal(parseAmount("1500000"), 1500000);
  assert.equal(parseAmount("1,500,000"), 1500000);
  assert.equal(parseAmount(" 1 500 000 "), 1500000);
  // Arabic-Indic digits: a cashier's keyboard produces them.
  assert.equal(parseAmount("٥٠٠٠٠٠"), 500000);
  assert.equal(parseAmount("۵۰۰۰۰۰"), 500000);
});

test("parseAmount truncates rather than rounding a fraction", () => {
  // The API refuses a fractional dinar. Truncating keeps the number sent equal
  // to the number the operator can see themselves having typed.
  assert.equal(parseAmount("1000.99"), 1000);
  assert.equal(parseAmount(""), 0);
  assert.equal(parseAmount("abc"), 0);
});

test("dateOnly drops the time and the timezone", () => {
  assert.equal(dateOnly("2026-03-15T09:30:00Z"), "2026-03-15");
  assert.equal(dateOnly("2026-03-15"), "2026-03-15");
  assert.equal(dateOnly(null), "");
});
