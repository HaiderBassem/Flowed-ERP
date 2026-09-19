/**
 * The icon set.
 *
 * Hand-written 24×24 stroke paths in the feather idiom, inlined so the CSP's
 * `default-src 'self'` never meets an icon font or a CDN sprite. Stroke icons
 * because the sidebar sits on a tinted parchment: filled glyphs at 16px read
 * as blots there, strokes read as drawing.
 *
 * Icons never carry meaning alone (§12 — state is never colour or shape
 * alone); every use sits beside its label.
 */

const PATHS: Record<string, string> = {
  // Work
  desk: "M2 9h20v11H2zM2 9l3-5h14l3 5M12 13h4",
  shift: "M12 3a9 9 0 1 0 9 9M12 7v5l3 3M21 3v5h-5",
  void: "M3 8h13a5 5 0 0 1 0 10H8M3 8l4-4M3 8l4 4",
  refund: "M21 8H8a5 5 0 0 0 0 10h8M21 8l-4-4M21 8l-4 4",
  discount: "M18 6 6 18M8.5 7.5a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0zM18.5 16.5a1.5 1.5 0 1 1-3 0 1.5 1.5 0 0 1 3 0z",
  approve: "M4 4h16v16H4zM8.5 12.5l2.5 2.5 5-5.5",
  // Records
  students: "M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM2 21v-1a7 7 0 0 1 14 0v1M17 8a3.5 3.5 0 0 1 0 7M22 21v-1a5.5 5.5 0 0 0-3-4.9",
  enroll: "M9 11a4 4 0 1 0 0-8 4 4 0 0 0 0 8zM2 21v-1a7 7 0 0 1 14 0v1M19 6v6M16 9h6",
  years: "M4 5h16v16H4zM4 10h16M9 3v4M15 3v4",
  imports: "M12 3v12M7 10l5 5 5-5M4 21h16",
  account: "M3 6h18v12H3zM3 10h18M7 15h4",
  receipt: "M6 2h12v20l-3-2-3 2-3-2-3 2zM9 7h6M9 11h6",
  // Bulk
  bulk: "M12 2 2 7l10 5 10-5-10-5zM2 12l10 5 10-5M2 17l10 5 10-5",
  promote: "M3 17l6-6 4 4 8-8M21 7v5M21 7h-5",
  // Config
  policy: "M6 2h9l5 5v15H6zM14 2v6h6M9 13h6M9 17h6",
  template: "M3 5h18M3 10h18M3 15h12M3 20h12",
  reference: "M12 8c5 0 9-1.3 9-3s-4-3-9-3-9 1.3-9 3 4 3 9 3zM3 5v14c0 1.7 4 3 9 3s9-1.3 9-3V5M3 12c0 1.7 4 3 9 3s9-1.3 9-3",
  // Governance
  reports: "M4 21V10M10 21V3M16 21v-8M22 21H2",
  scale: "M12 3v18M8 21h8M5 7l7-2 7 2M5 7l-3 7a4 4 0 0 0 6 0L5 7zM19 7l-3 7a4 4 0 0 0 6 0l-3-7z",
  shield: "M12 2 4 6v6c0 5 3.5 8.5 8 10 4.5-1.5 8-5 8-10V6l-8-4zM9 12l2.5 2.5L16 10",
  register: "M6 2h9l5 5v15H6zM10 14l4 4M14 14l-4 4",
  operators: "M10 8a3.5 3.5 0 1 0 0-7 3.5 3.5 0 0 0 0 7zM3 19v-1a6.5 6.5 0 0 1 11-4.7M17.5 15l.6 1.4 1.5.2-1.1 1 .3 1.5-1.3-.8-1.3.8.3-1.5-1.1-1 1.5-.2z",
  sponsors: "M7 11 12 6l5 5M2 12l5-5 5 5-5 5zM12 12l5-5 5 5-5 5z",
  bank: "M3 9l9-6 9 6M4 9v9M9 9v9M15 9v9M20 9v9M2 21h20M2 18h20",
  sessions: "M3 5h18v12H3zM8 21h8M12 17v4",
  backup: "M4 6c0-1.7 3.6-3 8-3s8 1.3 8 3-3.6 3-8 3-8-1.3-8-3zM4 6v6c0 1.7 3.6 3 8 3s8-1.3 8-3V6M4 12v6c0 1.7 3.6 3 8 3s8-1.3 8-3v-6",
  home: "M3 11 12 3l9 8M6 10v10h12V10",
  // Actions / status marks
  search: "M11 18a7 7 0 1 0 0-14 7 7 0 0 0 0 14zM21 21l-5-5",
  print: "M7 8V3h10v5M5 8h14a2 2 0 0 1 2 2v6h-4v4H7v-4H3v-6a2 2 0 0 1 2-2zM7 15h10",
  logout: "M14 4h6v16h-6M10 8l-4 4 4 4M6 12h11",
  gear: "M12 15a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM19 12a7 7 0 0 0-.1-1.2l2-1.6-2-3.4-2.4 1a7 7 0 0 0-2-1.2L14 3h-4l-.5 2.6a7 7 0 0 0-2 1.2l-2.4-1-2 3.4 2 1.6a7 7 0 0 0 0 2.4l-2 1.6 2 3.4 2.4-1a7 7 0 0 0 2 1.2L10 21h4l.5-2.6a7 7 0 0 0 2-1.2l2.4 1 2-3.4-2-1.6c.07-.4.1-.8.1-1.2z",
  identity: "M3 5h18v14H3zM7 13a2.5 2.5 0 1 0 0-5 2.5 2.5 0 0 0 0 5zM4 19a4.5 4.5 0 0 1 6 0M14 9h6M14 13h6",
  merge: "M7 3v6a6 6 0 0 0 6 6h4M17 3v6a6 6 0 0 1-6 6M17 15l4-3-4-3M12 21v-6",
  plan: "M8 3v4M16 3v4M4 5h16v16H4zM4 11h16M8 15h3",
  intent: "M2 8h20v10H2zM2 12h20M6 16h4M17 5V3M13 5.5 12 3M21 5.5 22 3",
  // Mirrors under RTL because the SVG inherits direction-agnostic coordinates
  // and we flip it in CSS with the layout.
  back: "M15 5l-7 7 7 7",
  collapse: "M9 4v16M3 4h18v16H3zM14 9l3 3-3 3",
};

export type IconName = keyof typeof PATHS;

export function Icon({
  name,
  size = 17,
  className,
}: {
  name: string;
  size?: number;
  className?: string;
}) {
  const d = PATHS[name];
  if (!d) return null;
  return (
    <svg
      className={`icon${className ? ` ${className}` : ""}`}
      width={size}
      height={size}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.7"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={d} />
    </svg>
  );
}
