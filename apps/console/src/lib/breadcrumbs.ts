import type { MessageKey } from './messages'

/** One step of the shell's breadcrumb, derived from the URL. */
export type Crumb =
  | { kind: 'page'; label: MessageKey; to?: '/' }
  | { kind: 'project'; project: string }
  | { kind: 'environment'; project: string; environment: string }
  | { kind: 'app'; project: string; environment: string; app: string }

const PAGES: Record<string, MessageKey> = {
  team: 'nav.team',
  tokens: 'nav.tokens',
  incidents: 'nav.incidents',
  webhooks: 'nav.webhooks',
  domains: 'nav.domains',
  audit: 'nav.audit',
  account: 'shell.account',
}

const decode = (segment: string) => {
  try {
    return decodeURIComponent(segment)
  } catch {
    return segment
  }
}

/** Projects › project › environment › app › Doctor, or the one top-level page. */
export function crumbsFor(pathname: string): Crumb[] {
  const [first, ...rest] = pathname.split('/').filter(Boolean).map(decode)
  if (first === undefined) return [{ kind: 'page', label: 'nav.projects' }]
  const page = PAGES[first]
  if (page) return [{ kind: 'page', label: page }]
  if (first !== 'projects') return []
  const [project, environment, app, tail] = rest
  const crumbs: Crumb[] = [{ kind: 'page', label: 'nav.projects', to: '/' }]
  if (project) crumbs.push({ kind: 'project', project })
  if (project && environment) crumbs.push({ kind: 'environment', project, environment })
  if (project && environment && app) crumbs.push({ kind: 'app', project, environment, app })
  if (app && tail === 'doctor') crumbs.push({ kind: 'page', label: 'doctor.title' })
  return crumbs
}
