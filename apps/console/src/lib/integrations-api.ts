/**
 * API calls of the organization's integrations (Git connections, GitHub App
 * installations, registry logins) and of an app's Git source. Only the
 * settings pages and the app page load this module.
 */
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { ok, unwrap } from './api'
import type {
  AppSource,
  CreateGitConnection,
  CreateOrgRegistry,
  PutSource,
  UpdateGitConnection,
  UpdateOrgRegistry,
} from './integrations'
import { ApiError } from './problem'

const connectionPath = (connection: string) => ({ params: { path: { connection } } })
const registryPath = (registry: string) => ({ params: { path: { registry } } })

// ---- Git connections ----

export const gitConnectionsQuery = queryOptions({
  queryKey: ['git-connections'],
  queryFn: () => unwrap(api.GET('/api/v1/git/connections')),
  retry: false,
})

export const createGitConnection = (body: CreateGitConnection) =>
  unwrap(api.POST('/api/v1/git/connections', { body }))

/** Check a token without saving it: whose it is, and its scopes. */
export const testNewGitConnection = (body: CreateGitConnection) =>
  unwrap(api.POST('/api/v1/git/connections/test', { body }))

export const updateGitConnection = (connection: string, body: UpdateGitConnection) =>
  unwrap(api.PATCH('/api/v1/git/connections/{connection}', { ...connectionPath(connection), body }))

/** 409 while an app source reads through it. */
export const deleteGitConnection = (connection: string) =>
  ok(api.DELETE('/api/v1/git/connections/{connection}', connectionPath(connection)))

export const testGitConnection = (connection: string) =>
  unwrap(api.POST('/api/v1/git/connections/{connection}/test', connectionPath(connection)))

/** A new webhook secret, shown once; the previous one stops working at once. */
export const rotateGitWebhookSecret = (connection: string) =>
  unwrap(api.POST('/api/v1/git/connections/{connection}/webhook-secret', connectionPath(connection)))

export const gitRepositoriesQuery = (connection: string, search: string, page: number) =>
  queryOptions({
    queryKey: ['git-connections', connection, 'repositories', search, page],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/git/connections/{connection}/repositories', {
          params: { path: { connection }, query: { search: search || undefined, page } },
        }),
      ),
    retry: false,
    staleTime: 60_000,
  })

export const gitBranchesQuery = (connection: string, repository: string) =>
  queryOptions({
    queryKey: ['git-connections', connection, 'branches', repository],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/git/connections/{connection}/branches', {
          params: { path: { connection }, query: { repository } },
        }),
      ),
    retry: false,
    staleTime: 60_000,
  })

// ---- GitHub App installations ----

export const gitInstallationsQuery = queryOptions({
  queryKey: ['git-installations'],
  queryFn: () => unwrap(api.GET('/api/v1/git/installations')),
  retry: false,
})

export const linkGitInstallation = (installationId: number) =>
  unwrap(api.POST('/api/v1/git/installations', { body: { installationId } }))

// ---- registry logins of the organization ----

export const orgRegistriesQuery = queryOptions({
  queryKey: ['org-registries'],
  queryFn: () => unwrap(api.GET('/api/v1/registries')),
  retry: false,
})

export const registryPresetsQuery = queryOptions({
  queryKey: ['org-registries', 'presets'],
  queryFn: () => unwrap(api.GET('/api/v1/registries/presets')),
  retry: false,
  staleTime: Number.POSITIVE_INFINITY,
})

export const createOrgRegistry = (body: CreateOrgRegistry) => unwrap(api.POST('/api/v1/registries', { body }))

/** Log in without saving the login. */
export const testNewOrgRegistry = (body: CreateOrgRegistry) =>
  unwrap(api.POST('/api/v1/registries/test', { body }))

/** A change; a new password rotates the login. */
export const updateOrgRegistry = (registry: string, body: UpdateOrgRegistry) =>
  unwrap(api.PUT('/api/v1/registries/{registry}', { ...registryPath(registry), body }))

export const deleteOrgRegistry = (registry: string) =>
  ok(api.DELETE('/api/v1/registries/{registry}', registryPath(registry)))

export const testOrgRegistry = (registry: string) =>
  unwrap(api.POST('/api/v1/registries/{registry}/test', registryPath(registry)))

// ---- the app's Git source ----

const appPath = (project: string, environment: string, app: string) => ({
  params: { path: { project, environment, app } },
})

/** The app's Git source; `null` when it has none (404). */
export const appSourceQuery = (project: string, environment: string, app: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'source'],
    queryFn: async (): Promise<AppSource | null> => {
      try {
        return await unwrap(
          api.GET(
            '/api/v1/projects/{project}/environments/{environment}/apps/{app}/source',
            appPath(project, environment, app),
          ),
        )
      } catch (error) {
        if (error instanceof ApiError && error.status === 404) return null
        throw error
      }
    },
    retry: false,
  })

export const putAppSource = (project: string, environment: string, app: string, body: PutSource) =>
  unwrap(
    api.PUT('/api/v1/projects/{project}/environments/{environment}/apps/{app}/source', {
      ...appPath(project, environment, app),
      body,
    }),
  )
