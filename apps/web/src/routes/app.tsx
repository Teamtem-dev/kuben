import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  Card,
  ConfirmDelete,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextArea,
  TextField,
} from '../components/ui'
import {
  appQuery,
  checkDomains,
  deleteApp,
  environmentsQuery,
  logsQuery,
  type PromoteResult,
  promoteApp,
  releasesQuery,
  restartApp,
  rollbackApp,
  runApp,
  type UpdateApp,
  updateApp,
  type Volume,
} from '../lib/api'
import { formatEnvLines, parseEnvLines } from '../lib/env'

const route = getRouteApi('/_authed/projects/$project/$environment/$app')

export function AppPage() {
  const { project, environment, app } = route.useParams()
  const { data } = useSuspenseQuery(appQuery(project, environment, app))
  const a = data.app
  const web = a.processes[0]
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })

  const update = useMutation({
    mutationFn: (body: UpdateApp) => updateApp(project, environment, app, body),
    onSuccess: refresh,
  })
  const restart = useMutation({ mutationFn: () => restartApp(project, environment, app), onSuccess: refresh })
  const run = useMutation({ mutationFn: () => runApp(project, environment, app) })
  const [deleteVolumes, setDeleteVolumes] = useState(false)
  const scheduled = a.processes.find((p) => p.schedule)
  const remove = useMutation({
    mutationFn: () => deleteApp(project, environment, app, deleteVolumes),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['apps', project, environment] })
      await navigate({ to: '/projects/$project/$environment', params: { project, environment } })
    },
  })

  return (
    <section className="space-y-6">
      <PageHeader
        crumbs={
          <>
            <Link to="/" className="hover:text-slate-200">
              Projects
            </Link>
            <span>/</span>
            <Link to="/projects/$project" params={{ project }} className="hover:text-slate-200">
              {project}
            </Link>
            <span>/</span>
            <Link
              to="/projects/$project/$environment"
              params={{ project, environment }}
              className="hover:text-slate-200"
            >
              {environment}
            </Link>
          </>
        }
        title={
          <span className="flex items-center gap-3">
            {a.name} <Status ready={a.ready} label={a.reason} />
          </span>
        }
        subtitle={
          a.url ? (
            <a href={a.url} target="_blank" rel="noreferrer" className="text-sky-300 hover:underline">
              {a.url}
            </a>
          ) : (
            'Not exposed'
          )
        }
        actions={
          <>
            {scheduled && (
              <Button variant="secondary" disabled={run.isPending} onClick={() => run.mutate()}>
                {run.isPending ? 'Starting…' : 'Run now'}
              </Button>
            )}
            <Button variant="secondary" disabled={restart.isPending} onClick={() => restart.mutate()}>
              {restart.isPending ? 'Restarting…' : 'Restart'}
            </Button>
          </>
        }
      />
      {scheduled && (
        <p className="text-slate-400 text-sm">
          Scheduled job: <code className="font-mono">{scheduled.schedule}</code>
          {run.data && <span className="text-emerald-300"> · started {run.data.job}</span>}
        </p>
      )}
      <ErrorNote error={run.error} />
      {!a.ready && a.message && (
        <p className="rounded-lg bg-amber-500/10 px-3 py-2 text-amber-200 text-sm">{a.message}</p>
      )}
      <ErrorNote error={restart.error} />

      <div className="grid gap-4 lg:grid-cols-2">
        <DeployCard
          image={a.image ?? ''}
          pending={update.isPending}
          error={update.error}
          onDeploy={(image) => update.mutate({ image })}
        />
        <ScaleCard
          min={web?.min_replicas ?? 1}
          max={web?.max_replicas ?? 1}
          size={web?.size ?? 'small'}
          pending={update.isPending}
