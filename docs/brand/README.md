# Flowed — brand identity

Everything on this page was read from the BrandSupply AI brand generator on
2026-08-14 and is recorded here so the identity does not live behind a URL that
can expire.

Source: <https://brandsupply.com/brand-generator/60c39f3d-bdfd-48bc-94e3-57191e0ca0ae>

The generated output is a **concept set**, not a finished identity — BrandSupply
says so itself and sells a designer refinement round for it. Read the caveats at
the bottom before putting any of this on something that prints.

---

## Positioning

> Flowed is the financial backbone of Iraqi higher education — a platform built
> for institutions that cannot afford ambiguity. Where every dirham is accounted
> for, every record is immutable, and every transaction earns trust. This is not
> software for consumers; this is infrastructure for institutions.

### Taglines

| | |
|---|---|
| **Every dirham. Accounted for.** | the generator's primary — see the currency note below |
| The ledger doesn't lie. | |
| Built for institutions. Not assumptions. | |

---

## Colour

| Role | Name | Hex | RGB |
|---|---|---|---|
| Primary | Deep Celadon | `#1C3A2F` | 28, 58, 47 |
| Accent | Sage Slate | `#3D6B5C` | 61, 107, 92 |
| Secondary | Antiqued Gold | `#C8A96E` | 200, 169, 110 |
| Neutral dark | Vault Black | `#1A1C1B` | 26, 28, 27 |
| Neutral light | Parchment White | `#F4F1EC` | 244, 241, 236 |

**Deep Celadon** — a dark, institutional forest green, referencing the permanence
of ledgers, the authority of government ministries, and the calm precision of
financial control. It reads as premium and serious without borrowing from generic
fintech blue, and holds dignity in both Arabic and Latin typographic contexts.
Green also carries cultural resonance in the Iraqi institutional landscape.

**Sage Slate** — a mid-tone green for interactive UI states, data highlights, and
active elements in dashboards. It stays inside the institutional palette while
providing enough contrast for WCAG-compliant enterprise interfaces in light and
dark modes. It reads as operational: the colour of a confirmation, a cleared
transaction, a reconciled ledger.

**Antiqued Gold** — a muted, aged gold. Not the shiny consumer gold of luxury
retail but the restrained gold of official seals, university crests and
ministerial letterheads. It communicates institutional prestige, long-term value
and financial authority, and pairs with the deep green to evoke the gravitas of a
central bank or a state university administration.

Machine-readable values are in [`tokens.css`](tokens.css).

---

## Typography

**Headings — Cormorant Garamond.** A high-contrast serif with editorial authority
and deep typographic roots in classical print. It brings the gravitas of
institutional documents — annual reports, university charters, government records
— into the digital interface. Its letterforms feel earned and permanent, not
trendy. It works in large display headings for dashboards and printed financial
reports, and its Latin letterforms complement Arabic script without competing
with it.

**Body — DM Sans.** A geometric humanist sans-serif with exceptional legibility at
small sizes in dense data environments: financial tables, audit trails, cashier
receipts. Neutral enough to defer to the heading face while staying modern and
highly functional. Its stroke consistency supports Arabic numeral alignment and
sits cleanly alongside Noto Sans Arabic for RTL rendering.

Both are open licence (SIL OFL) and can be self-hosted, which matters for desks
that are not reliably online.

Two constraints this repo already imposes on any type decision:

- Financial figures print **Western digits even on Arabic pages** — ٠ and 0 are
  confusable on a bad print, and a misread receipt becomes a dispute. This is why
  `--flowed-font-numeric` is the body face with tabular, lining figures.
- Cormorant Garamond has no Arabic cut. Arabic headings fall to Noto Sans Arabic
  set larger, rather than to a substitute serif that would not match.

---

## Voice — authoritative and precise

Flowed speaks like the most competent person in the room, not the loudest. It
communicates the way a senior financial controller would: measured, exact, never
wasteful with words. It does not perform warmth; it earns confidence. When it
says a record is immutable, you believe it. When it confirms a transaction, you
feel closure. It speaks to professionals who have been failed by bad software,
and it does not make promises it cannot document.

### Do

- Use declarative, transactional language: *"Payment recorded. Receipt issued."* —
  not *"Great! Your payment went through!"*
- Name the action and the outcome: *"Installment plan activated for student ID
  2024-0391 — 4 remaining payments"*
- Speak to the institutional role: financial managers as decision-makers,
  cashiers as operators, auditors as truth-seekers
- Use numeral and formatting conventions appropriate to Iraqi institutional
  contexts in all financial figures
- In error states, state exactly what failed and what must be done — never
  apologise vaguely

### Don't

- No exclamation points, emoji or celebratory microcopy in financial contexts. A
  payment confirmation is not an occasion; it is a record.
- No startup-speak: *seamless*, *game-changing*, *revolutionary*, *empower*,
  *unlock your potential*.
- Never be vague about a financial state. Ambiguity in a financial platform is a
  trust failure, not a UX choice.
- Do not infantilise the user with onboarding tooltips that assume incompetence.
  These are trained finance professionals.
- Never default to passive voice in system messages. *"The record was saved"*
  becomes *"System saved the record at 14:32 — Operator: Ahmed Al-Rashidi"*.

This aligns with the refusal style the codebase already uses: `shared.Error`
carries a stable machine code and a `WithDetail("remedy", ...)` that tells the
operator what to do instead. The voice guidance above is the sentence-level
version of the same rule.

---

## Logo concepts

Five AI-generated directions, all delivered as vector with the text already
converted to outlines — no font dependency at print time. Downloaded to
`logos/`, with 2000px-wide transparent PNG previews in `png/`.

| File | Mark |
|---|---|
| `logo-organic.svg` | **The generator's selected direction.** A calligraphic `F` built from three stacked leaf/ribbon strokes in Deep Celadon, Sage Slate and Antiqued Gold, beside a serif *Flowed* wordmark. |
| `logo-abstract.svg` | The same ribbon idea, flatter and more layered — four overlapping strokes reading as stacked ledger lines. |
| `logo-icon.svg` | A geometric `F` cut from squares with a single gold quarter-round counter. The most restrained of the five and the one that survives smallest. |
| `logo-geometric.svg` | An `F` assembled from a modular grid of squares, triangles and a gold half-disc. The most decorative; strongest as a pattern source, weakest at favicon size. |
| `logo-typographic.svg` | A rounded-square monogram containing a gold meander line, plus a standalone icon lock-up below the horizontal one. The only concept that ships a square app-icon form. |

All five use the palette above, with Deep Celadon dominant, Antiqued Gold as the
single accent, and Sage Slate only in the two ribbon marks.

**None of these is production-ready.** They are concept sketches: no clear-space
rule, no minimum size, no monochrome or reversed variant, no defined icon-only
lock-up for most of them. Pick a direction before commissioning any of that.

---

## Brand pattern

A 24 × 24 pt tile with a 2 pt dot at its centre, repeated. The dot is always Deep
Celadon; only its opacity changes with the ground, so the texture reads as one
mark everywhere rather than three different patterns.

| Ground | Dot opacity |
|---|---|
| Vault Black `#1A1C1B` | 0.25 |
| White | 0.08 |
| Parchment / tinted surface | 0.06 |
| Page background wash | 0.03 |

The tile is in [`pattern/dot-grid.svg`](pattern/dot-grid.svg); ready-to-use CSS
values are in `tokens.css` as `--flowed-pattern-on-{light,tint,dark}`.

---

## Touchpoints shown

The generator previews the identity on a business card (front and back), a social
profile, an email signature and a website hero. Every one of them is populated
with **BrandSupply's placeholder data** — "John Smith, Creative Director",
`hello@flowed.com`, `+31 6 12345678`, a Dutch phone number, 1,234 followers.
None of it is Flowed's. Treat the mockups as layout references only.

---

## Caveats worth acting on

**The currency in the tagline is wrong.** Iraq's currency is the **dinar** (IQD),
not the dirham — the dirham is the UAE and Moroccan unit. This system's money type
is `money.Amount`, whole dinars, and `money.SpellArabic` spells dinars on
receipts. "Every dirham. Accounted for." is therefore factually wrong about the
one thing the product is for, in the line that would appear on every page. The
fix is trivial in English — *"Every dinar. Accounted for."* — and the Arabic
should be set by someone who writes it natively rather than transliterated.
Of the three generated taglines, **"The ledger doesn't lie."** is the only one
that carries no currency risk at all.

**The SVGs carry a C2PA manifest.** Each file embeds an AI-provenance record in
its `<metadata>` block, which is roughly 25 KB of the ~27 KB file. That is
correct and should stay for the concept files, but a shipped logo should be a
re-drawn vector, not these with the metadata stripped.

**Licensing.** The generated concepts came from a free tier. Confirm what
BrandSupply's terms grant for commercial use before this appears on a receipt a
university issues — the paid refinement path (€199) exists partly to settle that.

---

## Files

```
docs/brand/
├── README.md                  this document
├── tokens.css                 colours, type stacks and the pattern, as CSS variables
├── logos/                     the five concepts as delivered (SVG, outlined text)
├── png/                       2000px-wide transparent renders of each
└── pattern/dot-grid.svg       the 24pt pattern tile
```
