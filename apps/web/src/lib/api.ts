import type { components } from '@kuben/api-client'
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { toApiError } from './problem'

type Schemas = components['schemas']
export type User = Schemas['UserDto']
export type Project = Schemas['ProjectDto']
export type Environment = Schemas['EnvironmentDto']
export type EnvType = Schemas['EnvType']
export type App = Schemas['AppDto']
export type AppDetail = Schemas['AppDetail']
export type Pod = Schemas['PodDto']
export type PodLogs = Schemas['PodLogs']
export type Secret = Schemas['SecretDto']
export type EnvVar = Schemas['EnvVarDto']
export type Volume = Schemas['VolumeDto']
export type CreateApp = Schemas['CreateApp']
export type UpdateApp = Schemas['UpdateApp']
export type Release = Schemas['ReleaseDto']
export type DomainCheck = Schemas['DomainCheck']
export type PromoteResult = Schemas['PromoteResult']
export type Template = Schemas['TemplateDto']
export type DeployedTemplate = Schemas['DeployedTemplate']
export type Token = Schemas['TokenDto']
export type CreateToken = Schemas['CreateToken']
export type CreatedToken = Schemas['CreatedToken']
export type Member = Schemas['MemberDto']
export type InvitedMember = Schemas['InvitedMember']
export type AuditEvent = Schemas['AuditEventDto']
export type AuditPage = Schemas['AuditPage']

interface Outcome<T> {
  data?: T
  error?: unknown
  response: Response
}

/** Resolve an openapi-fetch call to its data, or throw an `ApiError`. */
async function unwrap<T>(request: Promise<Outcome<T>>): Promise<T> {
  const { data, error, response } = await request
  if (!response.ok || data === undefined) throw toApiError(error, response.status)
  return data
}

/** For endpoints without a body (202/204). */
async function ok(request: Promise<Outcome<unknown>>): Promise<void> {
  const { error, response } = await request
  if (!response.ok) throw toApiError(error, response.status)
}

const appPath = (project: string, environment: string, app: string) => ({
  params: { path: { project, environment, app } },
})

// ---- session ----

/** The signed-in user, or `null` when there is no valid session. */
export const meQuery = queryOptions({
  queryKey: ['me'],
  queryFn: async (): Promise<User | null> => {
    const { data, error, response } = await api.GET('/api/v1/me')
    if (response.status === 401) return null
    if (!data) throw toApiError(error, response.status)
    return data
  },
  staleTime: 60_000,
})

export const login = (email: string, password: string) =>
  unwrap(api.POST('/api/v1/auth/login', { body: { email, password } }))

export const logout = () => ok(api.POST('/api/v1/auth/logout'))

export const changePassword = (current_password: string, new_password: string) =>
  ok(api.POST('/api/v1/me/password', { body: { current_password, new_password } }))

// ---- projects ----

export const projectsQuery = queryOptions({
  queryKey: ['projects'],
  queryFn: () => unwrap(api.GET('/api/v1/projects')),
})

export const projectQuery = (project: string) =>
  queryOptions({
    queryKey: ['projects', project],
    queryFn: () => unwrap(api.GET('/api/v1/projects/{project}', { params: { path: { project } } })),
  })

export const createProject = (body: Schemas['CreateProject']) =>
  unwrap(api.POST('/api/v1/projects', { body }))

export const deleteProject = (project: string) =>
  ok(api.DELETE('/api/v1/projects/{project}', { params: { path: { project } } }))

// ---- environments ----

export const environmentsQuery = (project: string) =>
  queryOptions({
    queryKey: ['environments', project],
    queryFn: () =>
      unwrap(api.GET('/api/v1/projects/{project}/environments', { params: { path: { project } } })),
  })

export const environmentQuery = (project: string, environment: string) =>
  queryOptions({
    queryKey: ['environments', project, environment],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}', {
          params: { path: { project, environment } },
        }),
      ),
  })

export const createEnvironment = (project: string, body: Schemas['CreateEnvironment']) =>
  unwrap(api.POST('/api/v1/projects/{project}/environments', { params: { path: { project } }, body }))

export const deleteEnvironment = (project: string, environment: string) =>
  ok(
    api.DELETE('/api/v1/projects/{project}/environments/{environment}', {
      params: { path: { project, environment } },
    }),
  )

// ---- apps ----

export const appsQuery = (project: string, environment: string) =>
  queryOptions({
    queryKey: ['apps', project, environment],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps', {
          params: { path: { project, environment } },
        }),
      ),
  })

export const appQuery = (project: string, environment: string, app: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app],
    queryFn: () =>
      unwrap(
        api.GET(
          '/api/v1/projects/{project}/environments/{environment}/apps/{app}',
          appPath(project, environment, app),
        ),
      ),
  })

export const logsQuery = (project: string, environment: string, app: string, tail: number) =>
  queryOptions({
    queryKey: ['logs', project, environment, app, tail],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps/{app}/logs', {
          params: { path: { project, environment, app }, query: { tail } },
        }),
      ),
    refetchInterval: 5_000,
  })

export const createApp = (project: string, environment: string, body: CreateApp) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps', {
      params: { path: { project, environment } },
      body,
    }),
  )

export const updateApp = (project: string, environment: string, app: string, body: UpdateApp) =>
  unwrap(
    api.PATCH('/api/v1/projects/{project}/environments/{environment}/apps/{app}', {
      ...appPath(project, environment, app),
      body,
    }),
  )

export const deleteApp = (project: string, environment: string, app: string, deleteVolumes = false) =>
  ok(
    api.DELETE('/api/v1/projects/{project}/environments/{environment}/apps/{app}', {
      params: { path: { project, environment, app }, query: { delete_volumes: deleteVolumes } },
    }),
  )

export const restartApp = (project: string, environment: string, app: string) =>
  ok(
    api.POST(
      '/api/v1/projects/{project}/environments/{environment}/apps/{app}/restart',
      appPath(project, environment, app),
    ),
  )

// ---- releases, runs, domains, promotion ----

export const releasesQuery = (project: string, environment: string, app: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'releases'],
    queryFn: () =>
      unwrap(
        api.GET(
          '/api/v1/projects/{project}/environments/{environment}/apps/{app}/releases',
          appPath(project, environment, app),
        ),
      ),
  })

export const rollbackApp = (project: string, environment: string, app: string, revision: number) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/rollback', {
      ...appPath(project, environment, app),
      body: { revision },
    }),
  )

export const runApp = (project: string, environment: string, app: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/run', {
      ...appPath(project, environment, app),
      body: {},
    }),
  )

export const checkDomains = (project: string, environment: string, app: string) =>
  unwrap(
    api.GET(
      '/api/v1/projects/{project}/environments/{environment}/apps/{app}/domains',
      appPath(project, environment, app),
    ),
  )

export const promoteApp = (
  project: string,
  environment: string,
  app: string,
  toEnvironment: string,
  dryRun: boolean,
) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/promote', {
      ...appPath(project, environment, app),
      body: { to_environment: toEnvironment, dry_run: dryRun },
    }),
  )

// ---- templates ----

export const templatesQuery = queryOptions({
  queryKey: ['templates'],
  queryFn: () => unwrap(api.GET('/api/v1/templates')),
  staleTime: Number.POSITIVE_INFINITY,
})

export const deployTemplate = (project: string, environment: string, template: string, name: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/templates/{template}', {
      params: { path: { project, environment, template } },
      body: { name },
    }),
  )

// ---- secrets ----

export const secretsQuery = (project: string, environment: string) =>
  queryOptions({
    queryKey: ['secrets', project, environment],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/secrets', {
          params: { path: { project, environment } },
        }),
      ),
  })

export const putSecret = (
  project: string,
  environment: string,
  secret: string,
  data: Record<string, string>,
) =>
  unwrap(
    api.PUT('/api/v1/projects/{project}/environments/{environment}/secrets/{secret}', {
      params: { path: { project, environment, secret } },
      body: { data },
    }),
