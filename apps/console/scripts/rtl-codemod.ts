#!/usr/bin/env bun
/**
 * Rewrites physical Tailwind classes in the generated shadcn/ui components
 * (src/components/ui/*.tsx) to logical ones, so the console lays out right
 * under dir="rtl":
 *
 *   ml-/mr- → ms-/me-          pl-/pr- → ps-/pe-          scroll-ml- → scroll-ms- …
 *   left-/right- → inset-s-/inset-e-                      text-left/right → text-start/end
 *   rounded-l/r → rounded-s/e  rounded-tl/tr/bl/br → rounded-ss/se/es/ee
 *   border-l/r → border-s/e    slide-in-from-left → slide-in-from-start …
 *
 * and, since `translate-x` has no logical form, gives every non-zero
 * `translate-x-*` its mirrored `rtl:` twin (e.g. `-translate-x-1/2` gains
 * `rtl:translate-x-1/2`).
 *
 * Left alone on purpose:
 *   - `data-[side=left|right]:…` — Radix popper sides are physical placements;
 *   - `data-[vaul-drawer-direction=…]:…` — vaul drags in physical directions;
 *   - class strings that already carry an `rtl:` translate (hand-tuned upstream).
 *
 * Idempotent: a second run changes nothing. Only strings in double quotes or
 * backticks are touched (shadcn writes classes that way), token by token.
 *
 *   bun scripts/rtl-codemod.ts            # rewrite in place
 *   bun scripts/rtl-codemod.ts --check    # exit 1 if anything would change
 */
import { readdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'

const side = (lr: string) => (lr === 'l' || lr === 'left' ? 's' : 'e')
const word = (lr: string) => (lr === 'left' ? 'start' : 'end')

/** Physical → logical, on a utility without its variants and sign. */
const RULES: [RegExp, (...m: string[]) => string][] = [
  [/^m([lr])-(.+)$/, (_, lr, v) => `m${side(lr)}-${v}`],
  [/^p([lr])-(.+)$/, (_, lr, v) => `p${side(lr)}-${v}`],
  [/^scroll-([mp])([lr])-(.+)$/, (_, mp, lr, v) => `scroll-${mp}${side(lr)}-${v}`],
  [/^(left|right)-(.+)$/, (_, lr, v) => `inset-${side(lr)}-${v}`],
  [/^text-(left|right)$/, (_, lr) => `text-${word(lr)}`],
  [/^rounded-([lr])(-.+)?$/, (_, lr, v = '') => `rounded-${side(lr)}${v}`],
  [/^rounded-([tb])([lr])(-.+)?$/, (_, tb, lr, v = '') => `rounded-${tb === 't' ? 's' : 'e'}${side(lr)}${v}`],
  [/^border-([lr])(-.+)?$/, (_, lr, v = '') => `border-${side(lr)}${v}`],
  [/^slide-(in-from|out-to)-(left|right)(-.+)?$/, (_, dir, lr, v = '') => `slide-${dir}-${word(lr)}${v}`],
  [/^(float|clear)-(left|right)$/, (_, p, lr) => `${p}-${word(lr)}`],
]

/** Splits `a:[&:x]:b-1` into its variants and utility (colons inside brackets stay). */
function split(token: string): { variants: string[]; utility: string } {
  const parts: string[] = []
  let depth = 0
  let start = 0
  for (let i = 0; i < token.length; i++) {
    const c = token[i]
    if (c === '[' || c === '(') depth++
    else if (c === ']' || c === ')') depth--
    else if (c === ':' && depth === 0) {
      parts.push(token.slice(start, i))
      start = i + 1
    }
  }
  parts.push(token.slice(start))
  const utility = parts.pop() ?? ''
  return { variants: parts, utility }
}

const physicalContext = (variants: string[]) =>
  variants.some((v) => v.startsWith('data-[side=') || v.includes('vaul-drawer-direction'))

/** Leading `!`/`-` and trailing `!` around a utility. */
function sign(utility: string): { pre: string; core: string; post: string } {
  const m = /^(!?-?)(.*?)(!?)$/.exec(utility)
  return { pre: m?.[1] ?? '', core: m?.[2] ?? utility, post: m?.[3] ?? '' }
}

export function logical(token: string): string {
  const { variants, utility } = split(token)
  if (physicalContext(variants)) return token
  const { pre, core, post } = sign(utility)
  for (const [re, to] of RULES) {
    const m = re.exec(core)
    if (m) return [...variants, `${pre}${to(...m)}${post}`].join(':')
  }
  return token
}

/** The mirrored `rtl:` twin of a non-zero `translate-x-*`, else undefined. */
export function mirroredTranslate(token: string): string | undefined {
  const { variants, utility } = split(token)
  if (physicalContext(variants) || variants.includes('rtl') || variants.includes('ltr')) return undefined
  const { pre, core, post } = sign(utility)
  const value = /^translate-x-(.+)$/.exec(core)?.[1]
  if (!value || value === '0') return undefined
  const negative = pre.endsWith('-')
  const bang = pre.startsWith('!') ? '!' : ''
  // `[-50%]` flips to `[50%]`; anything else flips through the sign prefix
  // (`-translate-x-[calc(…)]` is Tailwind's negated arbitrary value).
  const literal = /^\[-([^\]]+)\]$/.exec(value)
  const flipped = literal
    ? `${negative ? '-' : ''}translate-x-[${literal[1]}]`
    : `${negative ? '' : '-'}translate-x-${value}`
  return ['rtl', ...variants, `${bang}${flipped}${post}`].join(':')
}

/** One class string: tokens made logical, translate twins added. */
export function rewriteClasses(text: string): string {
  const pieces = text.split(/(\s+)/)
  const tokens = pieces.map((p) => (/^\s*$/.test(p) ? p : logical(p)))
  const hasRtlTranslate = tokens.some((t) => /(^|:)rtl:.*translate-x/.test(t))
  if (hasRtlTranslate) return tokens.join('')
  const present = new Set(tokens)
  const out: string[] = []
  for (const t of tokens) {
    out.push(t)
    if (/^\s*$/.test(t)) continue
    const twin = mirroredTranslate(t)
    if (twin && !present.has(twin)) out.push(' ', twin)
  }
  return out.join('')
}

/** Every double-quoted or backtick string in a source file. */
export function rewriteSource(source: string): string {
  return source.replace(/"((?:\\.|[^"\\\n])*)"|`((?:\\.|[^`\\])*)`/g, (whole, dq?: string, bt?: string) => {
    if (dq !== undefined) return `"${rewriteClasses(dq)}"`
    if (bt !== undefined) {
      // Keep `${…}` interpolations as they are; rewrite the text around them.
      const parts = bt.split(/(\$\{[^}]*\})/)
      return `\`${parts.map((p) => (p.startsWith('${') ? p : rewriteClasses(p))).join('')}\``
    }
    return whole
  })
}

if (import.meta.main) {
  const check = process.argv.includes('--check')
  const dir = join(import.meta.dir, '..', 'src', 'components', 'ui')
  let changed = 0
  for (const name of readdirSync(dir).filter((n) => n.endsWith('.tsx'))) {
    const path = join(dir, name)
    const before = readFileSync(path, 'utf8')
    const after = rewriteSource(before)
    if (after === before) continue
    changed++
    console.log(`${check ? 'would rewrite' : 'rewrote'} ${name}`)
    if (!check) writeFileSync(path, after)
  }
  console.log(`${changed} file(s) ${check ? 'not logical yet' : 'rewritten'}`)
  if (check && changed > 0) process.exit(1)
}
