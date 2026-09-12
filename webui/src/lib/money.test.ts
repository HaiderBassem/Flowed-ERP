import { describe, expect, it } from "vitest";

import golden from "./__fixtures__/tafqit.golden.json";
import {
  amount,
  format,
  formatCount,
  formatOrDash,
  formatWhileTyping,
  parseInput,
  spellArabic,
  spellArabicPlain,
  westernize,
} from "./money";

/**
 * The written amount is the thing that makes a receipt hard to alter, and this
 * file is the reason two implementations of it are tolerable.
 *
 * tafqit.golden.json is generated from internal/domain/money — the authority,
 * whose output is what gets printed. Every vector here came out of that
 * package, so a divergence in the port shows up as a failing test rather than
 * as a screen and a slip that disagree in an operator's hands.
 *
 * Regenerate after any change to arabic.go:
 *   go run ./.tafqitgen > webui/src/lib/__fixtures__/tafqit.golden.json
 */
interface Vector {
  n: string;
  spell: string;
  plain: string;
  fmt: string;
}

const vectors = golden as Vector[];

describe("tafqit — التفقيط", () => {
  it("has vectors covering the boundaries the grammar turns on", () => {
    expect(vectors.length).toBeGreaterThan(150);
  });

  it.each(vectors.map((v) => [v.n, v] as const))("spells %s as the server does", (_n, v) => {
    expect(spellArabic(amount(v.n))).toBe(v.spell);
  });

  it.each(vectors.map((v) => [v.n, v] as const))("spells %s plainly as the server does", (_n, v) => {
    expect(spellArabicPlain(amount(v.n))).toBe(v.plain);
  });

  it("keeps the closing marker unbreakable", () => {
    // Split across two lines on a narrow thermal roll, لا غير stops reading as
    // one phrase, and being an unmistakable end to the amount is its whole job.
    expect(spellArabic(amount(1_500_000))).toContain("لا غير");
  });

  it("does not count one and two out loud", () => {
    expect(spellArabic(amount(1))).not.toContain("واحد");
    expect(spellArabic(amount(2))).not.toContain("اثنان");
  });

  it("keeps a round hundred in the singular", () => {
    // The mistake an "eleven or more takes the accusative" implementation
    // makes: خمسمائة ألفاً instead of خمسمائة ألف.
    expect(spellArabic(amount(500_000))).toContain("خمسمائة ألف ");
    expect(spellArabic(amount(500_000))).not.toContain("ألفاً");
  });

  it("drops the nūn of a dual that governs its scale word", () => {
    expect(spellArabicPlain(amount(200_000))).toBe("مائتا ألف");
  });

  it("admits the gap beyond the scale words rather than inventing one", () => {
    expect(spellArabicPlain(amount(10n ** 18n))).toBe("مبلغ يتجاوز الحد القابل للكتابة");
  });
});

describe("format", () => {
  it.each(vectors.filter((v) => !v.n.startsWith("-")).map((v) => [v.n, v] as const))(
    "groups %s as the server does",
    (_n, v) => {
      expect(format(amount(v.n))).toBe(v.fmt);
    },
  );

  it("writes a true minus on screen where the server writes a hyphen on paper", () => {
    // U+2212 is the typographic minus §06 uses in its own examples; the server
    // string is what goes on the slip verbatim and is not reformatted here.
    expect(format(amount(-250_000))).toBe("−250,000");
    expect(format(amount(-250_000)).replace("−", "-")).toBe("-250,000");
  });

  it("writes the sign rather than relying on colour", () => {
    // Colour alone drops out under colour blindness and on the monochrome
    // printer these documents end up on.
    expect(format(amount(-1))).toMatch(/^−/);
    expect(format(amount(1), { sign: "always" })).toBe("+1");
  });

  it("distinguishes a computed zero from an absent figure", () => {
    expect(formatOrDash(amount(0))).toBe("0");
    expect(formatOrDash(null)).toBe("—");
    expect(formatCount(0)).toBe("0");
    expect(formatCount(null)).toBe("—");
  });

  it("carries amounts past the precision of a JS number", () => {
    const big = amount("9007199254740993"); // 2^53 + 1
    expect(format(big)).toBe("9,007,199,254,740,993");
    expect(Number(9007199254740993).toString()).toBe("9007199254740992"); // what a number would have done
  });
});

describe("input", () => {
  it("accepts Arabic-Indic digits and shows them Western", () => {
    expect(westernize("٧٠٠٠٠٠")).toBe("700000");
    const parsed = parseInput("٧٠٠٬٠٠٠");
    expect(parsed.ok && parsed.value).toBe(700_000n);
  });

  it("refuses a fraction as it is typed, with a reason", () => {
    const parsed = parseInput("500.50");
    expect(parsed.ok).toBe(false);
    expect(!parsed.ok && parsed.reason).toBe("fraction");
  });

  it("separates thousands while typing", () => {
    expect(formatWhileTyping("1500000")).toBe("1,500,000");
    expect(formatWhileTyping("١٥٠٠٠٠٠")).toBe("1,500,000");
  });

  it("treats an empty field as empty rather than as zero", () => {
    // Zero is a decision an operator makes; blank is one they have not made.
    const parsed = parseInput("  ");
    expect(!parsed.ok && parsed.reason).toBe("empty");
  });
});
