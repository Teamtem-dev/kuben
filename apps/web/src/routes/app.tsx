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
          onSave={(body) => update.mutate(body)}
        />
      </div>

      <div className="grid gap-4 lg:grid-cols-2">
        <ReleasesCard project={project} environment={environment} app={app} />
        <PromoteCard project={project} environment={environment} app={app} />
      </div>

      <Card title={`Pods (${data.pods.length})`}>
        {data.pods.length === 0 ? (
          <p className="text-slate-500 text-sm">No pods yet.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-slate-500 text-xs">
                <tr>
                  <th className="pb-2 font-medium">Pod</th>
                  <th className="pb-2 font-medium">Status</th>
                  <th className="pb-2 font-medium">Restarts</th>
                  <th className="pb-2 font-medium">Node</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-white/5">
                {data.pods.map((pod) => (
                  <tr key={pod.name}>
                    <td className="py-2 pe-4 font-mono text-xs">{pod.name}</td>
                    <td className="py-2 pe-4">
                      <Status ready={pod.ready} label={pod.reason ?? pod.phase} />
                    </td>
                    <td className="py-2 pe-4">{pod.restarts}</td>
                    <td className="py-2 font-mono text-slate-500 text-xs">{pod.node ?? '—'}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>

      <EnvCard
        text={formatEnvLines(a.env)}
        pending={update.isPending}
        error={update.error}
        onSave={(env) => update.mutate({ env })}
      />

      <Logs project={project} environment={environment} app={app} />

      <DomainsCard
        project={project}
        environment={environment}
        app={app}
        domains={a.domains}
        pending={update.isPending}
        onSave={(domains) => update.mutate({ domains })}
      />

      {a.volumes.length > 0 && <VolumesCard volumes={a.volumes} />}

      {a.volumes.length > 0 && (
        <label className="flex items-center gap-2 text-slate-400 text-sm">
          <input
            type="checkbox"
            checked={deleteVolumes}
            onChange={(e) => setDeleteVolumes(e.target.checked)}
          />
          Also delete the app's volumes when deleting it (irreversible). Otherwise they are kept.
        </label>
      )}
      <ConfirmDelete
        name={a.name}
        what="app"
        pending={remove.isPending}
        error={remove.error}
        onConfirm={() => remove.mutate()}
      />
    </section>
  )
}

function DeployCard({
  image,
  pending,
  error,
  onDeploy,
}: {
  image: string
  pending: boolean
