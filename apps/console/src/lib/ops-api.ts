/** API calls of the operational views (M5.6). */
import type { components } from '@kuben/api-client'
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { ok, unwrap } from './api'

type Schemas = components['schemas']
export type Incident = Schemas['IncidentDto']
export type Webhook = Schemas['EndpointDto']
export type Delivery = Schemas['DeliveryDto']
export type Claim = Schemas['ClaimDto']
export type DnsProvider = Schemas['DnsProviderDto']
export type Preview = Schemas['PreviewDto']
export type PreviewPolicy = Schemas['PreviewPolicyDto']
export type Detached = Schemas['DetachedAppDto']
export type ImagePolicy = Schemas['ImagePolicyDto']
export type Metrics = Schemas['MetricsDto']
export type StatusPageSettings = Schemas['StatusPageDto']

const app = (project: string, environment: string, name: string) => ({
  params: { path: { project, environment, app: name } },
})

// ---- incidents ----

export const incidentsQuery = (all: boolean) =>
  queryOptions({
    queryKey: ['incidents', all],
    queryFn: () => unwrap(api.GET('/api/v1/incidents', { params: { query: { all, limit: 200 } } })),
  })

export const acknowledgeIncident = (id: string) =>
  ok(api.POST('/api/v1/incidents/{id}/acknowledge', { params: { path: { id } } }))

export const resolveIncident = (id: string) =>
  ok(api.POST('/api/v1/incidents/{id}/resolve', { params: { path: { id } } }))

// ---- webhooks ----

export const webhooksQuery = queryOptions({
  queryKey: ['webhooks'],
  queryFn: () => unwrap(api.GET('/api/v1/webhooks')),
})

export const createWebhook = (name: string, url: string, events: string[]) =>
  unwrap(api.POST('/api/v1/webhooks', { body: { name, url, events } }))

export const disableWebhook = (id: string) =>
  ok(api.DELETE('/api/v1/webhooks/{id}', { params: { path: { id } } }))

export const pingWebhook = (id: string) =>
  ok(api.POST('/api/v1/webhooks/{id}/ping', { params: { path: { id } } }))

export const deliveriesQuery = (id: string) =>
  queryOptions({
    queryKey: ['webhooks', id, 'deliveries'],
    queryFn: () => unwrap(api.GET('/api/v1/webhooks/{id}/deliveries', { params: { path: { id } } })),
  })

export const retryDelivery = (id: string, delivery: string) =>
  ok(
    api.POST('/api/v1/webhooks/{id}/deliveries/{delivery}/retry', {
      params: { path: { id, delivery } },
    }),
  )

// ---- domains ----

export const claimsQuery = queryOptions({
  queryKey: ['domains'],
  queryFn: () => unwrap(api.GET('/api/v1/domains', { params: { query: { all: false } } })),
})

export const claimDomain = (domain: string) => unwrap(api.POST('/api/v1/domains', { body: { domain } }))

export const verifyClaim = (id: string, provider?: string) =>
  unwrap(
    api.POST('/api/v1/domains/{id}/verify', {
      params: { path: { id } },
      body: provider ? { provider } : {},
    }),
  )

export const revokeClaim = (id: string) =>
  ok(api.DELETE('/api/v1/domains/{id}', { params: { path: { id } } }))

export const dnsProvidersQuery = queryOptions({
  queryKey: ['dns-providers'],
  queryFn: () => unwrap(api.GET('/api/v1/dns-providers')),
})

export const addDnsProvider = (name: string, kind: string, token: string) =>
  unwrap(api.POST('/api/v1/dns-providers', { body: { name, kind, token } }))

export const removeDnsProvider = (id: string) =>
  ok(api.DELETE('/api/v1/dns-providers/{id}', { params: { path: { id } } }))

export const syncAppDns = (project: string, environment: string, name: string, provider: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/dns', {
      ...app(project, environment, name),
      body: { provider },
    }),
  )

// ---- previews ----

export const previewsQuery = (project: string, all: boolean) =>
  queryOptions({
    queryKey: ['previews', project, all],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/previews', {
          params: { path: { project }, query: { all } },
        }),
      ),
  })

export const previewPolicyQuery = (project: string) =>
  queryOptions({
    queryKey: ['previews', project, 'policy'],
    queryFn: () =>
      unwrap(api.GET('/api/v1/projects/{project}/previews/policy', { params: { path: { project } } })),
  })

export const putPreviewPolicy = (project: string, body: Schemas['PutPreviewPolicy']) =>
  unwrap(api.PUT('/api/v1/projects/{project}/previews/policy', { params: { path: { project } }, body }))

export const extendPreview = (project: string, environment: string, hours: number, keep: boolean) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/previews/{environment}/extend', {
      params: { path: { project, environment } },
      body: { hours, keep },
    }),
  )

export const destroyPreview = (project: string, environment: string) =>
  ok(
    api.DELETE('/api/v1/projects/{project}/previews/{environment}', {
      params: { path: { project, environment } },
    }),
  )

// ---- status page ----

export const statusPageQuery = (project: string) =>
  queryOptions({
    queryKey: ['status-page', project],
    queryFn: async () => {
      const { data, response } = await api.GET('/api/v1/projects/{project}/status-page', {
        params: { path: { project } },
      })
      return response.status === 404 ? null : (data ?? null)
    },
  })

export const putStatusPage = (project: string, body: Schemas['PutStatusPage']) =>
  unwrap(api.PUT('/api/v1/projects/{project}/status-page', { params: { path: { project } }, body }))

export const deleteStatusPage = (project: string) =>
  ok(api.DELETE('/api/v1/projects/{project}/status-page', { params: { path: { project } } }))

// ---- detached apps ----

export const detachedQuery = (project: string, environment: string) =>
  queryOptions({
    queryKey: ['detached', project, environment],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/detached', {
          params: { path: { project, environment } },
        }),
      ),
  })

export const detachedAppQuery = (project: string, environment: string, id: string) =>
  queryOptions({
    queryKey: ['detached', project, environment, id],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/detached/{id}', {
          params: { path: { project, environment, id } },
        }),
      ),
  })

export const releaseDetached = (project: string, environment: string, id: string) =>
  ok(
    api.POST('/api/v1/projects/{project}/environments/{environment}/detached/{id}/release', {
      params: { path: { project, environment, id } },
    }),
  )

export const detachApp = (project: string, environment: string, name: string, reason: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/detach', {
      ...app(project, environment, name),
      body: { confirm: name, reason },
    }),
  )

// ---- image policy and usage ----

export const imagePolicyQuery = (project: string, environment: string, name: string) =>
  queryOptions({
    queryKey: ['image-policy', project, environment, name],
    queryFn: async () => {
      const { data, response } = await api.GET(
        '/api/v1/projects/{project}/environments/{environment}/apps/{app}/image-policy',
        app(project, environment, name),
      )
      return response.status === 404 ? null : (data ?? null)
    },
  })

export const putImagePolicy = (
  project: string,
  environment: string,
  name: string,
  body: Schemas['PutImagePolicy'],
) =>
  unwrap(
    api.PUT('/api/v1/projects/{project}/environments/{environment}/apps/{app}/image-policy', {
      ...app(project, environment, name),
      body,
    }),
  )

export const deleteImagePolicy = (project: string, environment: string, name: string) =>
  ok(
    api.DELETE(
      '/api/v1/projects/{project}/environments/{environment}/apps/{app}/image-policy',
      app(project, environment, name),
    ),
  )

export const metricsQuery = (project: string, environment: string, name: string, window: '1h' | '7d') =>
  queryOptions({
    queryKey: ['metrics', project, environment, name, window],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps/{app}/metrics', {
          params: { path: { project, environment, app: name }, query: { window } },
        }),
      ),
    refetchInterval: 30_000,
  })
