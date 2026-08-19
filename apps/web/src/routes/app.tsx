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
  error: unknown
  onDeploy: (image: string) => void
}) {
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const next = String(new FormData(event.currentTarget).get('image') ?? '').trim()
    if (next) onDeploy(next)
  }
  return (
    <Card title="Image">
      <form onSubmit={onSubmit} className="space-y-3">
        <TextField key={image} label="Deploy image" name="image" defaultValue={image} required />
        <div className="flex items-center gap-3">
          <Button type="submit" disabled={pending}>
            Deploy
          </Button>
          <ErrorNote error={error} />
        </div>
      </form>
    </Card>
  )
}

function ScaleCard({
  min,
  max,
  size,
  pending,
  onSave,
}: {
  min: number
  max: number
  size: string
  pending: boolean
  onSave: (body: UpdateApp) => void
}) {
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const replicas = Number(form.get('replicas'))
    const maxReplicas = Number(form.get('max_replicas'))
    onSave({ replicas, max_replicas: Math.max(replicas, maxReplicas) })
  }
  return (
    <Card title={`Scale · ${size}`}>
      <form onSubmit={onSubmit} key={`${min}-${max}`} className="grid grid-cols-2 gap-3">
        <TextField label="Replicas" name="replicas" type="number" min={0} max={50} defaultValue={min} />
        <TextField
          label="Autoscale up to"
          name="max_replicas"
          type="number"
          min={0}
          max={50}
          defaultValue={max}
          hint="Equal to replicas disables autoscaling."
        />
        <div className="col-span-2">
          <Button type="submit" variant="secondary" disabled={pending}>
            Save
          </Button>
        </div>
      </form>
    </Card>
  )
}

function EnvCard({
  text,
  pending,
  error,
  onSave,
}: {
  text: string
  pending: boolean
  error: unknown
  onSave: (env: ReturnType<typeof parseEnvLines>['vars']) => void
}) {
  const [parseError, setParseError] = useState<string | null>(null)
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const { vars, errors } = parseEnvLines(String(new FormData(event.currentTarget).get('env') ?? ''))
    setParseError(errors.length ? errors.join('; ') : null)
    if (!errors.length) onSave(vars)
  }
  return (
    <Card title="Environment variables">
      <form onSubmit={onSubmit} className="space-y-3">
        <TextArea
          key={text}
          label="Variables"
          name="env"
          defaultValue={text}
          hint="Saving rolls out new pods. KEY=@secret/key references a secret."
        />
        <div className="flex items-center gap-3">
          <Button type="submit" variant="secondary" disabled={pending}>
            Save and roll out
          </Button>
          {parseError && <ErrorNote error={new Error(parseError)} />}
          <ErrorNote error={error} />
        </div>
      </form>
    </Card>
  )
}

function Logs({ project, environment, app }: { project: string; environment: string; app: string }) {
  const [tail, setTail] = useState(200)
  const logs = useQuery({ ...logsQuery(project, environment, app, tail), retry: false })
  return (
    <Card
      title="Logs"
      actions={
        <label className="flex items-center gap-2 text-slate-400 text-xs">
          Lines
          <select
            value={tail}
            onChange={(e) => setTail(Number(e.target.value))}
            className="rounded-md border border-white/10 bg-slate-950 px-2 py-1"
          >
            {[100, 200, 500, 1000].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
        </label>
      }
    >
      {logs.isError ? (
        <ErrorNote error={logs.error} />
      ) : !logs.data?.length ? (
        <p className="text-slate-500 text-sm">{logs.isLoading ? 'Loading…' : 'No pods to read logs from.'}</p>
      ) : (
        <div className="space-y-4">
          {logs.data.map((pod) => (
            <div key={pod.pod} className="space-y-1">
              <p className="font-mono text-slate-400 text-xs">{pod.pod}</p>
              {pod.error ? (
                <p className="text-amber-300 text-xs">{pod.error}</p>
              ) : (
                <pre className="max-h-96 overflow-auto rounded-lg bg-black/40 p-3 font-mono text-slate-300 text-xs leading-relaxed">
                  {pod.lines.join('\n') || '(no output yet)'}
                </pre>
              )}
            </div>
          ))}
        </div>
      )}
    </Card>
  )
}

function ReleasesCard({ project, environment, app }: { project: string; environment: string; app: string }) {
  const queryClient = useQueryClient()
  const releases = useQuery({ ...releasesQuery(project, environment, app), retry: false })
  const rollback = useMutation({
    mutationFn: (revision: number) => rollbackApp(project, environment, app, revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] }),
  })
  return (
    <Card title="Releases">
      {releases.isError ? (
        <ErrorNote error={releases.error} />
      ) : !releases.data?.length ? (
        <p className="text-slate-500 text-sm">No releases recorded yet.</p>
      ) : (
        <ul className="max-h-80 divide-y divide-white/5 overflow-y-auto">
          {releases.data.map((r) => (
            <li key={r.revision} className="flex items-center justify-between gap-3 py-2 text-sm">
              <div className="min-w-0">
                <p className="truncate">
                  <span className="font-mono">#{r.revision}</span> <Badge>{r.reason}</Badge>{' '}
                  <span className="font-mono text-slate-400 text-xs">{r.image ?? '—'}</span>
                </p>
                <p className="truncate text-slate-500 text-xs">
                  {new Date(r.created_at).toLocaleString()}
                  {r.actor ? ` · ${r.actor}` : ''}
                  {r.note ? ` · ${r.note}` : ''}
                </p>
              </div>
              {r.current ? (
                <span className="text-emerald-300 text-xs">current</span>
              ) : (
                <Button
                  variant="secondary"
                  disabled={rollback.isPending}
                  onClick={() => rollback.mutate(r.revision)}
                >
                  Roll back
                </Button>
              )}
            </li>
          ))}
        </ul>
      )}
      <ErrorNote error={rollback.error} />
    </Card>
  )
}

function PromoteCard({ project, environment, app }: { project: string; environment: string; app: string }) {
  const environments = useQuery(environmentsQuery(project))
  const targets = (environments.data ?? []).filter((e) => e.name !== environment)
  const [target, setTarget] = useState('')
  const [result, setResult] = useState<PromoteResult | null>(null)
  const promote = useMutation({
    mutationFn: (dryRun: boolean) => promoteApp(project, environment, app, target, dryRun),
    onSuccess: setResult,
  })
  if (targets.length === 0) return null
  const previewed = result?.dry_run === true
  return (
    <Card title="Promote">
      <div className="flex flex-wrap items-end gap-3">
        <Select
          label="Target environment"
          value={target}
          onChange={(e) => {
            setTarget(e.target.value)
            setResult(null)
          }}
        >
