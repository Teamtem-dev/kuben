// Renders public/og.png (1200×630) from an inline SVG with sharp.
// `bun run og` after changing the tagline; the file is committed because
// social previews are fetched by crawlers that never run a build.
import { mkdir } from 'node:fs/promises'
import path from 'node:path'
import sharp from 'sharp'

const out = path.join(import.meta.dirname, '..', 'public', 'og.png')

const svg = `
<svg xmlns="http://www.w3.org/2000/svg" width="1200" height="630" viewBox="0 0 1200 630">
  <defs>
    <linearGradient id="bg" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#0b0b0e"/>
      <stop offset="1" stop-color="#141216"/>
    </linearGradient>
    <radialGradient id="glow" cx="0.2" cy="0" r="0.8">
      <stop offset="0" stop-color="#f68b12" stop-opacity="0.32"/>
      <stop offset="1" stop-color="#f68b12" stop-opacity="0"/>
    </radialGradient>
    <linearGradient id="mark" x1="0" y1="0" x2="1" y2="1">
      <stop offset="0" stop-color="#ffc25c"/>
      <stop offset="1" stop-color="#f0640e"/>
    </linearGradient>
    <linearGradient id="accent" x1="0" y1="0" x2="1" y2="0">
      <stop offset="0" stop-color="#ffd99b"/>
      <stop offset="0.5" stop-color="#f68b12"/>
      <stop offset="1" stop-color="#ff8a3d"/>
    </linearGradient>
  </defs>
  <rect width="1200" height="630" fill="url(#bg)"/>
  <rect width="1200" height="630" fill="url(#glow)"/>

  <g transform="translate(96 92) scale(1.35)">
    <polygon points="32,6 54.5,19 54.5,45 32,58 9.5,45 9.5,19" fill="url(#mark)" stroke="url(#mark)" stroke-width="8" stroke-linejoin="round"/>
    <path d="M32 19 L43 25.5 L32 32 L21 25.5 Z" fill="#fff" fill-opacity="0.96"/>
    <path d="M21 25.5 L32 32 L32 45 L21 38.5 Z" fill="#fff" fill-opacity="0.5"/>
    <path d="M32 32 L43 25.5 L43 38.5 L32 45 Z" fill="#fff" fill-opacity="0.74"/>
  </g>
  <text x="200" y="152" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="56" font-weight="700" letter-spacing="-2" fill="#f4f4f5">kuben</text>

  <text x="96" y="330" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="78" font-weight="700" letter-spacing="-3" fill="#f4f4f5">A PaaS for your Kubernetes,</text>
  <text x="96" y="420" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="78" font-weight="700" letter-spacing="-3" fill="url(#accent)">in one binary.</text>

  <text x="96" y="500" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="28" fill="#a1a1aa">Isolated environments · automatic HTTPS · rollbacks · teams · audit log</text>

  <g transform="translate(96 552)">
    <rect width="292" height="44" rx="22" fill="#ffffff" fill-opacity="0.05" stroke="#ffffff" stroke-opacity="0.08"/>
    <text x="24" y="29" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="20" fill="#d4d4d8">kuben.teamtem.com</text>
  </g>
  <text x="1104" y="581" text-anchor="end" font-family="Inter, Helvetica Neue, Helvetica, Arial, sans-serif" font-size="20" fill="#71717a">a Teamtem project · Apache-2.0</text>
</svg>`

await mkdir(path.dirname(out), { recursive: true })
await sharp(Buffer.from(svg), { density: 144 }).png({ compressionLevel: 9 }).toFile(out)
console.log(`wrote ${out}`)
