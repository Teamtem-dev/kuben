# Patches to the generated shadcn/ui components

Everything in this directory was written by the shadcn CLI
(`shadcn@4.21.0`, style `new-york`, base colour `neutral`, see
`apps/console/components.json`) and is otherwise kept as generated, so a
later `shadcn add <name> --overwrite` shows only upstream changes. After
re-adding a component, run the RTL codemod and re-apply the patches below.

```sh
cd apps/console
bunx --bun shadcn@latest add <name> --overwrite   # BUN_CONFIG_REGISTRY=https://registry.npmjs.org/
bun scripts/rtl-codemod.ts                        # then re-apply the patches listed here
```

Biome neither formats nor organises imports here, and a few style/a11y rules
that the upstream code trips are off for this directory (`biome.json`,
first override), so the files stay byte-for-byte comparable with upstream.

## 1. RTL codemod (all files) — mass patch

- **What:** `scripts/rtl-codemod.ts` rewrites physical Tailwind classes to
  logical ones (`ml-`→`ms-`, `pr-`→`pe-`, `left-`/`right-`→`inset-s-`/`inset-e-`,
  `text-left`→`text-start`, `rounded-l`→`rounded-s`, `border-r`→`border-e`,
  `slide-in-from-left`→`slide-in-from-start`, …) and gives every non-zero
  `translate-x-*` a mirrored `rtl:` twin. Radix popper sides
  (`data-[side=left]:…`) and vaul drawer directions stay physical: they are
  physical placements.
- **Why:** the console runs in Persian with `dir="rtl"`; the sidebar and every
  inset, padding and border must follow the reading direction.
- **Check:** `bun scripts/rtl-codemod.ts --check`; `src/lib/rtl.test.ts` fails
  when a file is not logical.

## 2. `chart.tsx` — `ChartStyle`

- **What:** `ChartStyle` no longer renders `<style dangerouslySetInnerHTML>`.
  It renders a hidden `<span>` and, from a layout effect, sets the
  `--color-<key>` variables on the enclosing `[data-chart]` element with
  `element.style.setProperty` (CSSOM), choosing the light or dark value by
  matching the `THEMES` selectors against the chart's ancestors, and again
  whenever `<html>`'s class changes.
- **Why:** Kuben serves the console with `style-src 'self'` (no
  `'unsafe-inline'`); an injected `<style>` element is blocked, CSSOM writes
  are not.

## 3. `progress.tsx` — mirrored in RTL

- **What:** `rtl:-scale-x-100` on the root.
- **Why:** the indicator is moved with an inline `translateX(-…%)`, which the
  codemod cannot make logical; mirroring the bar makes it fill from the start
  side in RTL.

## Not patched here, handled elsewhere

- `sonner.tsx` reads the theme from `next-themes`, which Kuben does not use;
  the shell passes `theme` from Kuben's preferences (`<Toaster theme=…>`).
- Libraries that inject `<style>` at run time (sonner, vaul, Radix Select and
  Scroll Area, input-otp, react-style-singleton) are handled at build time in
  `vite-plugins/csp-styles.ts` and by the `react-style-singleton` alias in
  `vite.config.ts`, not by editing these files.
- The generated components import `cn` from the `cn` package (shadcn's
  compiled replacement for clsx + tailwind-merge); `src/lib/utils.ts`
  re-exports it for hand-written code.
