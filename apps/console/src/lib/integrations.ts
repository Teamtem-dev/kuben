/**
 * Pure helpers of the organization's integrations: Git provider connections
 * (GitHub, GitLab, Gitea and Forgejo by token), registry logins shared by
 * every environment, and the app's "connect a repository" flow.
 */
import type { components } from '@kuben/api-client'
import type { Tone } from './ops'

type Schemas = components['schemas']
export type GitProvider = Schemas['GitProviderDto']
export type GitConnection = Schemas['GitConnectionDto']
export type CreateGitConnection = Schemas['CreateGitConnection']
export type UpdateGitConnection = Schemas['UpdateGitConnection']
export type GitConnectionCheck = Schemas['GitConnectionCheckDto']
export type GitRepository = Schemas['GitRepositoryDto']
export type GitRepositoryList = Schemas['GitRepositoryListDto']
export type GitBranch = Schemas['GitBranchDto']
export type Installation = Schemas['InstallationDto']
export type RegistryPresetId = Schemas['RegistryPresetIdDto']
export type RegistryPreset = Schemas['RegistryPresetDto']
export type OrgRegistry = Schemas['OrgRegistryDto']
export type CreateOrgRegistry = Schemas['CreateOrgRegistry']
export type UpdateOrgRegistry = Schemas['UpdateOrgRegistry']
export type RegistryCheck = Schemas['RegistryCheckDto']
export type AppSource = Schemas['SourceDto']
export type PutSource = Schemas['PutSource']
export type Strategy = Schemas['StrategyDto']

/**
 * A card of the "add connection" dialog. Forgejo is a fork of Gitea with
 * the same API: it is saved as a `gitea` connection.
 */
export type ProviderCard = 'github' | 'gitlab' | 'gitea' | 'forgejo'

export const PROVIDER_CARDS: readonly ProviderCard[] = ['github', 'gitlab', 'gitea', 'forgejo']

interface CardInfo {
  provider: GitProvider
  /** The URL the form starts with; empty when the user must name their server. */
  url: string
  /** What the URL field shows as an example. */
  placeholder: string
}

const CARDS: Record<ProviderCard, CardInfo> = {
  github: { provider: 'github', url: 'https://api.github.com', placeholder: 'https://api.github.com' },
  gitlab: { provider: 'gitlab', url: 'https://gitlab.com', placeholder: 'https://gitlab.example.com' },
  gitea: { provider: 'gitea', url: '', placeholder: 'https://gitea.example.com' },
  forgejo: { provider: 'gitea', url: 'https://codeberg.org', placeholder: 'https://git.example.com' },
}

export const cardProvider = (card: ProviderCard): GitProvider => CARDS[card].provider
export const cardUrl = (card: ProviderCard): string => CARDS[card].url
export const cardPlaceholder = (card: ProviderCard): string => CARDS[card].placeholder

/** Gitea and Forgejo have no public service everyone uses: their URL is required. */
export const cardNeedsUrl = (card: ProviderCard): boolean => CARDS[card].provider === 'gitea'

/** The card a saved connection belongs to (a `gitea` one could be Forgejo; Gitea is the honest guess). */
export function cardOf(connection: Pick<GitConnection, 'provider' | 'baseUrl'>): ProviderCard {
  if (connection.provider !== 'gitea') return connection.provider
  return /codeberg\.org|forgejo/i.test(connection.baseUrl) ? 'forgejo' : 'gitea'
}

/** Names the API accepts: lowercase letters, digits and `-`, starting with a letter or digit. */
export const NAME_PATTERN = '[a-z0-9][a-z0-9-]{0,62}'

/** A name to start from: the card, and the host when it is not the provider's public service. */
export function suggestName(card: ProviderCard, url: string): string {
  let host = ''
  try {
    host = new URL(url).hostname
  } catch {
    host = ''
  }
  const publicHost = host === 'api.github.com' || host === 'gitlab.com' || host === ''
  const raw = publicHost ? card : `${card}-${host.replace(/^(www|api)\./, '')}`
  return (
    raw
      .toLowerCase()
      .replace(/[^a-z0-9-]+/g, '-')
      .replace(/-+/g, '-')
      .replace(/^-|-$/g, '')
      .slice(0, 63) || card
  )
}

export interface ConnectionForm {
  card: ProviderCard
  name: string
  url: string
  token: string
  defaultBranch: string
}

/** The body of a create (and of a test before it), from the dialog's fields. */
export function connectionBody(form: ConnectionForm): CreateGitConnection {
  const body: CreateGitConnection = {
    provider: cardProvider(form.card),
    name: form.name.trim(),
    token: form.token.trim(),
  }
  const url = form.url.trim().replace(/\/+$/, '')
  if (url) body.baseUrl = url
  const branch = form.defaultBranch.trim()
  if (branch) body.defaultBranch = branch
  return body
}

/**
 * The body of an edit: only what changed. An empty token keeps the saved
 * one; an empty default branch clears it.
 */
export function connectionChange(
  saved: Pick<GitConnection, 'name' | 'baseUrl' | 'defaultBranch'>,
  form: Pick<ConnectionForm, 'name' | 'url' | 'token' | 'defaultBranch'>,
): UpdateGitConnection {
  const change: UpdateGitConnection = {}
  const name = form.name.trim()
  const url = form.url.trim().replace(/\/+$/, '')
  const token = form.token.trim()
  const branch = form.defaultBranch.trim()
  if (name && name !== saved.name) change.name = name
  if (url && url !== saved.baseUrl) change.baseUrl = url
  if (token) change.token = token
  if (branch !== (saved.defaultBranch ?? '')) change.defaultBranch = branch || null
  return change
}

/** The webhook address as the provider needs it: absolute, on the console's own origin when the API gave a path. */
export function absoluteUrl(url: string, origin: string): string {
  if (/^https?:\/\//i.test(url)) return url
  return `${origin.replace(/\/+$/, '')}/${url.replace(/^\/+/, '')}`
}

/** A check's colour: fine, fine with scopes missing, or failed. */
export function checkTone(check: Pick<GitConnectionCheck, 'ok' | 'missingScopes'>): Tone {
  if (!check.ok) return 'danger'
  return check.missingScopes.length > 0 ? 'warning' : 'success'
}

/** How a saved connection or registry login stands after its last check. */
export function lastCheckTone(item: { lastCheckedAt?: number | null; lastError?: string | null }): Tone {
  if (item.lastError) return 'danger'
  return item.lastCheckedAt == null ? 'neutral' : 'success'
}

// ---- registries ----

/** Presets whose server the user names (Harbor, a custom registry). */
export const presetNeedsServer = (preset: Pick<RegistryPreset, 'server'> | undefined) => !preset?.server

/** `https://registry.example.com:5000/` as image references name it: `registry.example.com:5000`. */
export function registryHost(server: string): string {
  return server
    .trim()
    .replace(/^[a-z]+:\/\//i, '')
    .replace(/\/.*$/, '')
    .toLowerCase()
}

export interface RegistryForm {
  preset: RegistryPresetId
  name: string
  server: string
  username: string
  password: string
}

/** The body of a create (and of a test before it). */
export function registryBody(form: RegistryForm, preset: RegistryPreset | undefined): CreateOrgRegistry {
  const body: CreateOrgRegistry = {
    preset: form.preset,
    name: form.name.trim(),
    username: form.username.trim(),
    password: form.password,
  }
  const server = registryHost(form.server)
  if (presetNeedsServer(preset) || (server && server !== preset?.server)) body.server = server
  return body
}

// ---- the app's source ----

/** What the "connect a repository" dialog reads through: a token connection, or a GitHub App installation. */
export type SourceVia = { kind: 'connection'; id: string } | { kind: 'installation'; id: number }

export const viaValue = (via: SourceVia) => `${via.kind}:${via.id}`

/** The value of the source select back to what it names; `null` for anything else. */
export function viaFrom(value: string): SourceVia | null {
  const [kind, id] = value.split(/:(.*)/s)
  if (kind === 'connection' && id) return { kind, id }
  if (kind === 'installation' && id && /^\d+$/.test(id)) return { kind, id: Number(id) }
  return null
}

/** `owner/name` (GitLab: `group/subgroup/name`), or `null` when `value` is not one. */
export function repositoryFrom(value: string): string | null {
  const text = value
    .trim()
    .replace(/^https?:\/\/[^/]+\//i, '')
    .replace(/\.git$/i, '')
    .replace(/\/+$/, '')
  return /^[\w.-]+(\/[\w.-]+)+$/.test(text) ? text : null
}

export interface SourceForm {
  via: SourceVia
  repository: string
  branch: string
  strategy: Strategy
  context: string
  dockerfile: string
  imageRepository: string
}

/** The body of PUT …/source. */
export function sourceBody(form: SourceForm): PutSource {
  const body: PutSource = {
    repository: form.repository.trim(),
    branch: form.branch.trim(),
    strategy: form.strategy,
    imageRepository: form.imageRepository.trim(),
  }
  if (form.via.kind === 'connection') body.connection = form.via.id
  else body.installationId = form.via.id
  const context = form.context.trim()
  if (context) body.context = context
  const dockerfile = form.dockerfile.trim()
  if (form.strategy === 'dockerfile' && dockerfile) body.dockerfile = dockerfile
  return body
}

/** The branch to preselect: the one already chosen when it exists, else the repository's default, else the first. */
export function pickBranch(branches: readonly GitBranch[], current: string, fallback?: string | null) {
  if (current && branches.some((b) => b.name === current)) return current
  const byDefault = branches.find((b) => b.default)?.name
  if (byDefault) return byDefault
  if (fallback && branches.some((b) => b.name === fallback)) return fallback
  return branches[0]?.name ?? ''
}
