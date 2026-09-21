import { describe, expect, test } from 'bun:test'
import { readdirSync, readFileSync, statSync } from 'node:fs'
import { join } from 'node:path'

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
