/**
 * API calls of the operational controls (M4.9): owners, change freezes,
 * alert silences, paused delivery and emergency rollbacks; and of the
 * organization settings: CI trust policies and single sign-on.
 */
import type { components } from '@kuben/api-client'
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { ok, unwrap } from './api'

type Schemas = components['schemas']
export type Owner = Schemas['OwnerDto']
export type ControlWindow = Schemas['WindowDto']
export type CreateWindow = Schemas['CreateWindow']
export type CiPolicy = Schemas['CiPolicyDto']
export type CreateCiPolicy = Schemas['CreateCiPolicy']

const envPath = (project: string, environment: string) => ({ params: { path: { project, environment } } })
const appPath = (project: string, environment: string, app: string) => ({
  params: { path: { project, environment, app } },
})

// ---- owners ----

export const projectOwnerQuery = (project: string) =>
  queryOptions({
    queryKey: ['owner', project],
    queryFn: async (): Promise<Owner | null> =>
      (await unwrap(api.GET('/api/v1/projects/{project}/owner', { params: { path: { project } } }))) ?? null,
  })

export const putProjectOwner = (project: string, body: Owner) =>
  unwrap(api.PUT('/api/v1/projects/{project}/owner', { params: { path: { project } }, body }))

/** Who answers for an application, in every environment of the project. */
export const appOwnerQuery = (project: string, app: string) =>
  queryOptions({
    queryKey: ['owner', project, app],
    queryFn: async (): Promise<Owner | null> =>
      (await unwrap(
        api.GET('/api/v1/projects/{project}/applications/{app}/owner', {
          params: { path: { project, app } },
        }),
      )) ?? null,
  })

export const putAppOwner = (project: string, app: string, body: Owner) =>
  unwrap(
    api.PUT('/api/v1/projects/{project}/applications/{app}/owner', {
      params: { path: { project, app } },
      body,
    }),
  )

// ---- freezes and silences ----

export const freezesQuery = (project: string, environment: string, all: boolean) =>
  queryOptions({
    queryKey: ['freezes', project, environment, all],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/freezes', {
          params: { path: { project, environment }, query: { all } },
        }),
      ),
  })

export const createFreeze = (project: string, environment: string, body: CreateWindow) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/freezes', {
      ...envPath(project, environment),
      body,
    }),
  )

export const liftFreeze = (project: string, environment: string, id: string) =>
  ok(
    api.DELETE('/api/v1/projects/{project}/environments/{environment}/freezes/{id}', {
      params: { path: { project, environment, id } },
    }),
  )

export const silencesQuery = (project: string, environment: string, all: boolean) =>
  queryOptions({
    queryKey: ['silences', project, environment, all],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/silences', {
          params: { path: { project, environment }, query: { all } },
        }),
      ),
  })

export const createSilence = (project: string, environment: string, body: CreateWindow) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/silences', {
      ...envPath(project, environment),
      body,
    }),
  )

export const liftSilence = (project: string, environment: string, id: string) =>
  ok(
    api.DELETE('/api/v1/projects/{project}/environments/{environment}/silences/{id}', {
      params: { path: { project, environment, id } },
    }),
  )

// ---- delivery ----

export const pauseApp = (project: string, environment: string, app: string, reason: string) =>
  ok(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/pause', {
      ...appPath(project, environment, app),
      body: { reason },
    }),
  )

export const resumeApp = (project: string, environment: string, app: string) =>
  ok(
    api.POST(
      '/api/v1/projects/{project}/environments/{environment}/apps/{app}/resume',
      appPath(project, environment, app),
    ),
  )

/** Back to the newest earlier release that ran, past approvals, a freeze, the scan gate and a pause. */
export const emergencyRollback = (project: string, environment: string, app: string, reason: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/emergency-rollback', {
      ...appPath(project, environment, app),
      body: { reason },
    }),
  )

// ---- CI trust policies ----

export const ciPoliciesQuery = queryOptions({
  queryKey: ['ci-policies'],
  queryFn: () => unwrap(api.GET('/api/v1/ci/trust-policies')),
  retry: false,
})

export const createCiPolicy = (body: CreateCiPolicy) =>
  unwrap(api.POST('/api/v1/ci/trust-policies', { body }))

export const revokeCiPolicy = (policy: string) =>
  ok(api.DELETE('/api/v1/ci/trust-policies/{policy}', { params: { path: { policy } } }))
