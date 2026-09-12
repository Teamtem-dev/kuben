// @ts-check
import sitemap from '@astrojs/sitemap'
import starlight from '@astrojs/starlight'
import tailwindcss from '@tailwindcss/vite'
import { defineConfig } from 'astro/config'
import icon from 'astro-icon'
import starlightBlog from 'starlight-blog'
import starlightImageZoom from 'starlight-image-zoom'
import starlightLinksValidator from 'starlight-links-validator'
import starlightLlmsTxt from 'starlight-llms-txt'
import starlightOpenAPI, { openAPISidebarGroups } from 'starlight-openapi'

const site = 'https://kuben.teamtem.com'
const repo = 'https://github.com/Teamtem-dev/kuben'

export default defineConfig({
  site,
  trailingSlash: 'always',
  // Astro's default `<img>` handling; Starlight adds `loading="lazy"` where it matters.
  prefetch: { prefetchAll: true, defaultStrategy: 'viewport' },
  integrations: [
    starlight({
      title: 'Kuben',
      description:
        'Kuben is a Kubernetes PaaS in a single binary: deploy container images into isolated environments from a web UI or a REST API, with zero-downtime rollouts, automatic HTTPS, teams and an audit log.',
      favicon: '/favicon.svg',
      lastUpdated: true,
      editLink: { baseUrl: `${repo}/edit/main/apps/site/` },
      social: [
        { icon: 'github', label: 'GitHub', href: repo },
        { icon: 'blueSky', label: 'Teamtem', href: 'https://teamtem.com' },
      ],
      customCss: ['./src/styles/global.css'],
      components: {
        Header: './src/components/starlight/Header.astro',
        Footer: './src/components/starlight/Footer.astro',
        Sidebar: './src/components/starlight/Sidebar.astro',
      },
      head: [
        { tag: 'meta', attrs: { name: 'theme-color', content: '#0b0b0d' } },
        { tag: 'meta', attrs: { property: 'og:image', content: `${site}/og.png` } },
        { tag: 'meta', attrs: { property: 'og:image:width', content: '1200' } },
        { tag: 'meta', attrs: { property: 'og:image:height', content: '630' } },
        { tag: 'meta', attrs: { name: 'twitter:card', content: 'summary_large_image' } },
        { tag: 'meta', attrs: { name: 'twitter:image', content: `${site}/og.png` } },
      ],
      expressiveCode: {
        themes: ['github-dark-default', 'github-light'],
        styleOverrides: {
          borderRadius: '0.9rem',
          borderWidth: '0px',
          codeFontFamily: 'var(--sl-font-mono)',
          frames: { shadowColor: 'transparent', editorActiveTabIndicatorTopColor: 'var(--sl-color-accent)' },
        },
      },
      sidebar: [
        {
          label: 'Start here',
          items: [
            { label: 'What is Kuben?', slug: 'docs' },
            { label: 'Quickstart', slug: 'docs/getting-started/quickstart' },
            { label: 'Deploy your first app', slug: 'docs/getting-started/first-app' },
            { label: 'Concepts', slug: 'docs/getting-started/concepts' },
            { label: 'Install the binary', slug: 'docs/getting-started/binary' },
          ],
        },
        {
          label: 'Guides',
          items: [{ autogenerate: { directory: 'docs/guides' } }],
        },
        {
          label: 'Operations',
          items: [{ autogenerate: { directory: 'docs/operations' } }],
        },
        {
          label: 'Reference',
          items: [
            { label: 'CLI', slug: 'docs/reference/cli' },
            { label: 'Configuration', slug: 'docs/reference/configuration' },
            { label: 'Helm chart values', slug: 'docs/reference/helm-values' },
            { label: 'Custom resources', slug: 'docs/reference/custom-resources' },
            { label: 'Template catalogue', slug: 'docs/reference/templates' },
            ...openAPISidebarGroups,
          ],
        },
        {
          label: 'Contributing',
          items: [{ autogenerate: { directory: 'docs/contributing' } }],
        },
        { label: 'Roadmap', slug: 'docs/roadmap' },
      ],
      plugins: [
        starlightOpenAPI([
          {
            base: 'docs/reference/api',
            schema: '../../packages/api-client/openapi.json',
            sidebar: { label: 'REST API', collapsed: true },
          },
        ]),
        starlightBlog({
          title: 'Blog',
          prefix: 'blog',
          // The site header carries the Blog link; no extra link in the title area.
          navigation: 'none',
          postCount: 8,
          recentPostCount: 5,
          metrics: { readingTime: true, words: false },
          authors: {
            teamtem: { name: 'Teamtem', title: 'Kuben maintainers', url: 'https://teamtem.com' },
          },
        }),
        starlightImageZoom(),
        starlightLlmsTxt({
          projectName: 'Kuben',
          description: 'Kuben is a Kubernetes PaaS in a single binary, built in Rust by Teamtem.',
          exclude: ['blog/**'],
        }),
        starlightLinksValidator({
          errorOnRelativeLinks: false,
          // Astro pages outside the docs collection.
          exclude: ['/docs/reference/api/**', '/enterprise/', '/enterprise/**', '/changelog/'],
        }),
      ],
    }),
    icon({ include: { lucide: ['*'], 'simple-icons': ['github', 'kubernetes', 'rust', 'helm', 'react'] } }),
    sitemap(),
  ],
  vite: {
    plugins: [tailwindcss()],
    // `satteri` (the Markdown engine starlight-openapi renders with) loads a
    // native binding by name. Keep it out of the prerender bundle so that
    // lookup happens from the package itself, which works with Bun's isolated
    // node_modules layout.
    ssr: { external: ['satteri'] },
  },
})
