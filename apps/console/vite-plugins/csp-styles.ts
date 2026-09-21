/**
 * Build-time fixes for libraries that add `<style>` elements at run time,
 * which Kuben's content security policy (`style-src 'self'`, no
 * 'unsafe-inline') blocks. Each rule takes the CSS out of the library's code
 * and hands it to Vite as an ordinary stylesheet (emitted as a file and
 * loaded with <link>, which the policy allows), then removes the injection.
 *
 * A rule whose pattern is missing from its module fails the build: after a
 * library upgrade the rule must be checked again rather than silently stop
 * working. `react-style-singleton` is handled separately, by an alias to
 * src/lib/style-singleton.ts (constructed stylesheets).
 */
import type { Plugin } from 'vite'

export interface CspRule {
  /** Short name, also the virtual stylesheet's name. */
  name: string
  /** Module ids (resolved file paths) the rule applies to. */
  module: RegExp
  /** Code without the injection and the CSS it would have injected. */
  rewrite: (code: string) => { code: string; css: string } | undefined
}

/** tsup's `__insertCSS("…")`, run when the module loads (sonner, vaul). */
function insertCss(code: string) {
  const call = /__insertCSS\(("(?:\\.|[^"\\])*")\)/.exec(code)
  if (!call?.[1]) return undefined
  const css = JSON.parse(call[1]) as string
  return { code: code.replace(call[0], 'void 0'), css }
}

/** Radix's `jsx("style", { dangerouslySetInnerHTML: { __html: `…` }, nonce })`. */
function radixStyle(code: string) {
  const element =
    /jsx\(\s*"style",\s*\{\s*dangerouslySetInnerHTML:\s*\{\s*__html:\s*`([^`$]*)`\s*\},\s*nonce\s*\}\s*\)/
  const found = element.exec(code)
  if (!found?.[1]) return undefined
  return { code: code.replace(found[0], 'null'), css: found[1] }
}

/** The rules input-otp adds through an (empty) `<style>` element's CSSOM. */
const OTP_HIDDEN =
  'background: transparent !important; color: transparent !important; border-color: transparent !important; opacity: 0 !important; box-shadow: none !important; -webkit-box-shadow: none !important; -webkit-text-fill-color: transparent !important;'
const INPUT_OTP_CSS = `[data-input-otp]::selection { background: transparent !important; color: transparent !important; }
[data-input-otp]:autofill { ${OTP_HIDDEN} }
[data-input-otp]:-webkit-autofill { ${OTP_HIDDEN} }
@supports (-webkit-touch-callout: none) { [data-input-otp] { letter-spacing: -.6em !important; font-weight: 100 !important; font-stretch: ultra-condensed; font-optical-sizing: none !important; left: -1px !important; right: 1px !important; } }
[data-input-otp] + * { pointer-events: all !important; }`

function inputOtp(code: string) {
  const guard = '!document.getElementById("input-otp-style")'
  if (!code.includes(guard)) return undefined
  return { code: code.replace(guard, 'false'), css: INPUT_OTP_CSS }
}

export const CSP_RULES: CspRule[] = [
  { name: 'sonner', module: /\/sonner\/dist\/index\.mjs$/, rewrite: insertCss },
  { name: 'vaul', module: /\/vaul\/dist\/index\.mjs$/, rewrite: insertCss },
  { name: 'radix-select', module: /\/@radix-ui\/react-select\/dist\/index\.mjs$/, rewrite: radixStyle },
  {
    name: 'radix-scroll-area',
    module: /\/@radix-ui\/react-scroll-area\/dist\/index\.mjs$/,
    rewrite: radixStyle,
  },
  { name: 'input-otp', module: /\/input-otp\/dist\/index\.mjs$/, rewrite: inputOtp },
]

const PREFIX = 'virtual:csp-styles/'

export function cspStyles(rules: CspRule[] = CSP_RULES): Plugin {
  const css = new Map<string, string>()
  return {
    name: 'kuben:csp-styles',
    // Dev serves without the policy and pre-bundles dependencies as they are.
    apply: 'build',
    enforce: 'pre',
    resolveId(id) {
      return id.startsWith(PREFIX) ? `\0${id}` : undefined
    },
    load(id) {
      if (!id.startsWith(`\0${PREFIX}`)) return undefined
      const name = id.slice(PREFIX.length + 1, -'.css'.length)
      return css.get(name) ?? ''
    },
    transform(code, id) {
      const path = id.split('?')[0] ?? id
      const rule = rules.find((r) => r.module.test(path))
      if (!rule) return undefined
      const result = rule.rewrite(code)
      if (!result) this.error(`csp-styles: the ${rule.name} rule no longer matches ${path}; re-check it`)
      css.set(rule.name, result.css)
      return { code: `import "${PREFIX}${rule.name}.css";\n${result.code}`, map: null }
    },
  }
}
