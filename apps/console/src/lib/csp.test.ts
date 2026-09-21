import { describe, expect, test } from 'bun:test'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { CSP_RULES } from '../../vite-plugins/csp-styles'

// Kuben serves the console with `script-src 'self'; style-src 'self'`:
// nothing inline may creep in.
const root = join(import.meta.dir, '..', '..')
const ui = join(root, 'src', 'components', 'ui')

function sources(dir: string): string[] {
  return readdirSync(dir).flatMap((name) => {
    const path = join(dir, name)
    if (statSync(path).isDirectory()) return sources(path)
    return /\.tsx?$/.test(name) && !name.endsWith('.test.ts') ? [path] : []
  })
}

describe('content security policy', () => {
  test('index.html has no inline script or style', () => {
    const html = readFileSync(join(root, 'index.html'), 'utf8')
    expect(html).not.toMatch(/<script(?![^>]*\bsrc=)[^>]*>/)
    expect(html).not.toMatch(/<style|\sstyle=/)
  })

  test('no component writes inline markup or a <style> element', () => {
    for (const file of sources(join(root, 'src'))) {
      const text = readFileSync(file, 'utf8')
      expect({ file, inline: /dangerouslySetInnerHTML|<style[\s>]/.test(text) }).toEqual({
        file,
        inline: false,
      })
    }
  })

  // React applies `style` props through the CSSOM, which the policy allows;
  // the generated shadcn/ui components use them. Kuben's own code keeps to
  // classes.
  test('only the shadcn/ui components use style props', () => {
    for (const file of sources(join(root, 'src')).filter((f) => !f.startsWith(ui))) {
      const text = readFileSync(file, 'utf8')
      expect({ file, style: /\sstyle=\{/.test(text) }).toEqual({ file, style: false })
    }
  })
})

/** A dependency's ES module entry as installed (isolated linker: resolved from its dependant). */
function installed(pkg: string, from: string, entry: string): string {
  const manifest = Bun.resolveSync(`${pkg}/package.json`, from)
  return join(dirname(manifest), entry)
}

describe('build-time fixes for injected styles (vite-plugins/csp-styles.ts)', () => {
  const radix = dirname(Bun.resolveSync('radix-ui/package.json', root))
  const modules: Record<string, string> = {
    sonner: installed('sonner', root, 'dist/index.mjs'),
    vaul: installed('vaul', root, 'dist/index.mjs'),
    'radix-select': installed('@radix-ui/react-select', radix, 'dist/index.mjs'),
    'radix-scroll-area': installed('@radix-ui/react-scroll-area', radix, 'dist/index.mjs'),
    'input-otp': installed('input-otp', root, 'dist/index.mjs'),
  }

  for (const rule of CSP_RULES) {
    test(`${rule.name}: matches the installed module and removes the injection`, () => {
      const path = modules[rule.name]
      if (!path) throw new Error(`no module listed for ${rule.name}`)
      expect(rule.module.test(path)).toBe(true)
      const result = rule.rewrite(readFileSync(path, 'utf8'))
      expect(result).toBeDefined()
      expect(result?.css.length).toBeGreaterThan(20)
      expect(result?.code).not.toMatch(/__insertCSS\("|dangerouslySetInnerHTML: \{\s*__html: `\[data-radix/)
      expect(result?.code).not.toContain('!document.getElementById("input-otp-style")')
    })
  }
})
