/**
 * API calls of the app page's delivery views: a run's approval and the
 * decisions on it, and the builds of a Git-sourced app. Only the app page
 * loads this module, so it stays out of the entry chunk.
 */
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { unwrap } from './api'
import { type Approval, type DecideRequest, isAwaitingApproval, isFinalBuild } from './delivery'

const runPath = (project: string, environment: string, app: string, run: string) => ({
  params: { path: { project, environment, app, run } },
})

const buildPath = (project: string, environment: string, app: string, build: string) => ({
  params: { path: { project, environment, app, build } },
})

// ---- approvals ----

/** Who asked, how many approvals it needs and has, the plan hash and whether the caller may decide. */
export const approvalQuery = (project: string, environment: string, app: string, run: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'deployments', run, 'approval'],
    queryFn: (): Promise<Approval> =>
      unwrap(
        api.GET(
          '/api/v1/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/approval',
          runPath(project, environment, app, run),
        ),
      ),
    retry: false,
    // Others decide too: follow the run while it waits.
    refetchInterval: (query) =>
      query.state.data && isAwaitingApproval(query.state.data.phase) ? 5_000 : false,
  })

/** The run as admission left it: its generation, plan hash and what admission could not check. */
export const deploymentRunQuery = (project: string, environment: string, app: string, run: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'deployments', run],
    queryFn: () =>
      unwrap(
        api.GET(
          '/api/v1/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}',
          runPath(project, environment, app, run),
        ),
      ),
    retry: false,
  })

export const approveDeployment = (
  project: string,
  environment: string,
  app: string,
  run: string,
  body: DecideRequest,
) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/approve', {
      ...runPath(project, environment, app, run),
      body,
    }),
  )

export const rejectDeployment = (
  project: string,
  environment: string,
  app: string,
  run: string,
  body: DecideRequest,
) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/deployments/{run}/reject', {
      ...runPath(project, environment, app, run),
      body,
    }),
  )

// ---- builds ----

export const buildsQuery = (project: string, environment: string, app: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'builds'],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps/{app}/builds', {
          params: { path: { project, environment, app }, query: { limit: 50 } },
        }),
      ),
    retry: false,
    // While a build is under way, follow it.
    refetchInterval: (query) => (query.state.data?.some((b) => !isFinalBuild(b.phase)) ? 3_000 : false),
  })

export const buildQuery = (project: string, environment: string, app: string, build: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'builds', build],
    queryFn: () =>
      unwrap(
        api.GET(
          '/api/v1/projects/{project}/environments/{environment}/apps/{app}/builds/{build}',
          buildPath(project, environment, app, build),
        ),
      ),
    retry: false,
    refetchInterval: (query) => (query.state.data && !isFinalBuild(query.state.data.phase) ? 3_000 : false),
  })

/** Ask a build to stop (202: the build as it stands; poll it). */
export const cancelBuild = (project: string, environment: string, app: string, build: string) =>
  unwrap(
    api.POST(
      '/api/v1/projects/{project}/environments/{environment}/apps/{app}/builds/{build}/cancel',
      buildPath(project, environment, app, build),
    ),
  )

/** The scans of the app's current release (and which of its images have an SBOM). */
export const appScansQuery = (project: string, environment: string, app: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'scans'],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps/{app}/scans', {
          params: { path: { project, environment, app } },
        }),
      ),
    retry: false,
  })

/** Build the source's branch head now (202: the sync, and the build once it is queued). */
export const triggerBuild = (project: string, environment: string, app: string) =>
  unwrap(
    api.POST('/api/v1/projects/{project}/environments/{environment}/apps/{app}/builds', {
      params: { path: { project, environment, app } },
    }),
  )

/** A build's log as text: the build pod's while it runs, the kept tail after. */
export const buildLogQuery = (project: string, environment: string, app: string, build: string) =>
  queryOptions({
    queryKey: ['app', project, environment, app, 'builds', build, 'logs'],
    queryFn: () =>
      unwrap(
        api.GET('/api/v1/projects/{project}/environments/{environment}/apps/{app}/builds/{build}/logs', {
          ...buildPath(project, environment, app, build),
          parseAs: 'text',
        }),
      ),
    retry: false,
  })
