import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  Card,
  ConfirmDelete,
  Empty,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextArea,
  TextField,
} from '../components/ui'
import {
  appsQuery,
  createApp,
  type DeployedTemplate,
  deleteEnvironment,
  deleteSecret,
  deployTemplate,
  environmentQuery,
  projectQuery,
  putSecret,
  secretsQuery,
  templatesQuery,
} from '../lib/api'
import { parseEnvLines } from '../lib/env'

const route = getRouteApi('/_authed/projects/$project/$environment')

export function EnvironmentPage() {
  const { project, environment } = route.useParams()
  const { data: p } = useSuspenseQuery(projectQuery(project))
  const { data: env } = useSuspenseQuery(environmentQuery(project, environment))
  const { data: apps } = useSuspenseQuery(appsQuery(project, environment))
  const [deploying, setDeploying] = useState(false)
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const remove = useMutation({
    mutationFn: () => deleteEnvironment(project, environment),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['environments', project] })
      await navigate({ to: '/projects/$project', params: { project } })
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
              {p.display_name}
            </Link>
          </>
        }
        title={
          <span className="flex items-center gap-3">
            {env.name} <Badge>{env.env_type}</Badge>
          </span>
        }
        subtitle={<span className="font-mono">{env.namespace}</span>}
        actions={
          <Button
            variant={deploying ? 'secondary' : 'primary'}
            onClick={() => setDeploying((v) => !v)}
            disabled={env.deleting}
          >
            {deploying ? 'Cancel' : 'Deploy app'}
          </Button>
        }
      />

      {env.deleting && (
        <p role="status" className="rounded-lg bg-amber-500/10 px-3 py-2 text-amber-200 text-sm">
          This environment is being deleted
          {env.deletion_scheduled_at
            ? ` — its namespace is purged at ${new Date(env.deletion_scheduled_at).toLocaleString()}`
            : ''}
          .
        </p>
      )}

      {deploying && (
        <DeployForm project={project} environment={environment} onDone={() => setDeploying(false)} />
      )}

      {apps.length === 0 ? (
        <Empty>
          No apps yet. Deploy any container image; Kuben creates the Deployment, Service and route.
        </Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2">
          {apps.map((a) => (
            <li key={a.name}>
              <Link
                to="/projects/$project/$environment/$app"
                params={{ project, environment, app: a.name }}
                className="block rounded-xl border border-white/10 bg-slate-900/50 p-4 transition hover:border-sky-400/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="truncate font-medium">{a.name}</span>
                  <Status ready={a.ready} label={a.reason} />
                </div>
                <p className="mt-1 truncate font-mono text-slate-500 text-xs">{a.image ?? a.git_repo}</p>
                {a.url && <p className="mt-1 truncate text-sky-300 text-xs">{a.url}</p>}
                {!a.ready && a.message && <p className="mt-2 text-amber-300/80 text-xs">{a.message}</p>}
              </Link>
            </li>
          ))}
        </ul>
      )}

      <Templates project={project} environment={environment} />

      <Secrets project={project} environment={environment} />

      <div className="border-white/10 border-t pt-6">
        <ConfirmDelete
          name={env.name}
          what="environment"
          pending={remove.isPending}
          error={remove.error}
          onConfirm={() => remove.mutate()}
        />
      </div>
    </section>
  )
}

function DeployForm({
  project,
  environment,
  onDone,
}: {
  project: string
  environment: string
  onDone: () => void
}) {
  const queryClient = useQueryClient()
  const [envError, setEnvError] = useState<string | null>(null)
  const mutation = useMutation({
    mutationFn: (body: Parameters<typeof createApp>[2]) => createApp(project, environment, body),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['apps', project, environment] })
      onDone()
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const text = (key: string) => String(form.get(key) ?? '').trim()
    const { vars, errors } = parseEnvLines(text('env'))
    setEnvError(errors.length ? errors.join('; ') : null)
    if (errors.length) return
    const port = text('port')
    const max = text('max_replicas')
    const [mountPath, size] = text('volume').split(':')
    const schedule = text('schedule')
    mutation.mutate({
      name: text('name'),
      image: text('image'),
      port: port ? Number(port) : null,
      replicas: Number(text('replicas') || '1'),
      max_replicas: max ? Number(max) : null,
      size: text('size') || 'small',
      env: vars,
      domains: text('domains')
        .split(/[\s,]+/)
        .filter(Boolean),
      health_check_path: text('health') || null,
      schedule: schedule || null,
      protocol: text('protocol') === 'tcp' ? 'tcp' : 'http',
      volumes: mountPath ? [{ name: 'data', mount_path: mountPath.trim(), size: size?.trim() || '1Gi' }] : [],
    })
  }

  return (
    <form
      onSubmit={onSubmit}
      className="grid gap-4 rounded-xl border border-white/10 bg-slate-900/50 p-4 sm:grid-cols-3"
    >
      <TextField
        label="Name"
        name="name"
        required
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={40}
        placeholder="api"
      />
      <div className="sm:col-span-2">
        <TextField label="Image" name="image" required placeholder="ghcr.io/acme/api:1.4.2" />
      </div>
      <TextField
        label="Port"
        name="port"
        type="number"
        min={1}
        max={65535}
        placeholder="8080 (empty for workers)"
      />
      <TextField label="Replicas" name="replicas" type="number" min={0} max={50} defaultValue={1} />
      <TextField
        label="Autoscale up to"
        name="max_replicas"
        type="number"
        min={1}
        max={50}
        placeholder="optional"
      />
      <Select label="Size" name="size" defaultValue="small">
        <option value="nano">nano — 50m / 128Mi</option>
        <option value="small">small — 100m / 256Mi</option>
        <option value="medium">medium — 250m / 1Gi</option>
        <option value="large">large — 1 CPU / 4Gi</option>
      </Select>
      <TextField label="Health check path" name="health" placeholder="/healthz" />
      <TextField label="Domains" name="domains" placeholder="api.example.com" />
      <Select label="Protocol" name="protocol" defaultValue="http">
        <option value="http">http — public route</option>
        <option value="tcp">tcp — internal only (databases)</option>
      </Select>
      <TextField label="Schedule (cron)" name="schedule" placeholder="0 3 * * * (runs as a job; no port)" />
      <TextField
        label="Volume"
        name="volume"
        placeholder="/data:5Gi (single replica)"
        pattern="/[^:]+(:[0-9]+(Ki|Mi|Gi|Ti))?"
      />
      <div className="sm:col-span-3">
        <TextArea
          label="Environment variables"
          name="env"
          placeholder={'LOG_LEVEL=info\nDATABASE_URL=@db/url'}
          hint="One KEY=value per line; KEY=@secret/key references a secret."
        />
      </div>
      <div className="flex items-center gap-3 sm:col-span-3">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? 'Deploying…' : 'Deploy'}
        </Button>
        {envError && <ErrorNote error={new Error(envError)} />}
        <ErrorNote error={mutation.error} />
      </div>
    </form>
  )
}

function Secrets({ project, environment }: { project: string; environment: string }) {
  const queryClient = useQueryClient()
  const secrets = useQuery({ ...secretsQuery(project, environment), retry: false })
  const [formError, setFormError] = useState<string | null>(null)
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['secrets', project, environment] })
  const save = useMutation({
    mutationFn: ({ name, data }: { name: string; data: Record<string, string> }) =>
      putSecret(project, environment, name, data),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: (name: string) => deleteSecret(project, environment, name),
    onSuccess: refresh,
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const { vars, errors } = parseEnvLines(String(form.get('data') ?? ''))
    const data = Object.fromEntries(vars.map((v) => [v.name, v.value ?? '']))
    if (errors.length || vars.length === 0) {
      setFormError(errors.length ? errors.join('; ') : 'Add at least one KEY=value line.')
      return
    }
    setFormError(null)
    save.mutate(
      { name: String(form.get('name') ?? '').trim(), data },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <Card title="Secrets">
      <div className="space-y-4">
        {secrets.isError ? (
          <ErrorNote error={secrets.error} />
        ) : secrets.data?.length ? (
