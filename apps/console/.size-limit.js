// Bundle budgets (brotli), read from the build (`bun run build` first):
// - initial JS: what index.html loads, the entry chunk and the chunks it
//   statically imports (its modulepreload links), together ≤ 200 kB;
// - every chunk loaded later (a page, or a library only pages use) ≤ 80 kB;
// - all CSS ≤ 25 kB.
import { existsSync, readdirSync, readFileSync } from 'node:fs'

if (!existsSync('dist/index.html'))
  throw new Error('size-limit: no dist/index.html; run `bun run build` first')

const html = readFileSync('dist/index.html', 'utf8')
const initial = [...new Set([...html.matchAll(/"\/(assets\/[^"]+\.js)"/g)].map((m) => `dist/${m[1]}`))]
const lazy = readdirSync('dist/assets')
  .filter((name) => name.endsWith('.js'))
  .map((name) => `dist/assets/${name}`)
  .filter((path) => !initial.includes(path))

export default [
  { name: 'Initial JS (entry + static imports)', path: initial, limit: '200 kB' },
  ...lazy.map((path) => ({
    name: `Lazy chunk ${path.slice('dist/assets/'.length).replace(/-[\w-]{8}\.js$/, '')}`,
    path,
    limit: '80 kB',
  })),
  { name: 'CSS', path: 'dist/assets/*.css', limit: '25 kB' },
]
