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
  deleteRegistryLogin,
  deleteSecret,
  deployTemplate,
  environmentQuery,
  projectQuery,
  putRegistryLogin,
  putSecret,
  registriesQuery,
  secretsQuery,
  templatesQuery,
} from '../lib/api'
import { parseEnvLines } from '../lib/env'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'
import { DetachedCard } from './ops/environment-ops'

const route = getRouteApi('/_authed/projects/$project/$environment')

export function EnvironmentPage() {
  const { project, environment } = route.useParams()
  const { t, locale } = usePrefs()
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
            <Link to="/" className="hover:text-fg">
              {t('projects.title')}
            </Link>
            <span>/</span>
            <Link to="/projects/$project" params={{ project }} className="hover:text-fg">
              <span dir="auto">{p.display_name}</span>
            </Link>
          </>
        }
        title={
          <span className="flex items-center gap-3">
            {env.name} <Badge>{env.env_type}</Badge>
          </span>
        }
        subtitle={
          <span dir="ltr" className="font-mono">
            {env.namespace}
          </span>
        }
        actions={
          <Button
            variant={deploying ? 'secondary' : 'primary'}
            onClick={() => setDeploying((v) => !v)}
            disabled={env.deleting}
          >
            {deploying ? t('ui.cancel') : t('environment.deployApp')}
          </Button>
        }
      />

      {env.deleting && (
        <p role="status" className="rounded-lg bg-warn/10 px-3 py-2 text-warn text-sm">
          {t('environment.deleting')}
          {env.deletion_scheduled_at
            ? ` — ${fill(t('environment.purgedAt'), { date: new Date(env.deletion_scheduled_at).toLocaleString(locale) })}`
            : ''}
          .
        </p>
      )}

      {deploying && (
        <DeployForm project={project} environment={environment} onDone={() => setDeploying(false)} />
      )}

      {apps.length === 0 ? (
        <Empty>{t('environment.empty')}</Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2">
          {apps.map((a) => (
            <li key={a.name}>
              <Link
                to="/projects/$project/$environment/$app"
                params={{ project, environment, app: a.name }}
                className="block rounded-xl border border-line bg-surface p-4 transition hover:border-brand/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span dir="auto" className="truncate font-medium">
                    {a.name}
                  </span>
                  <Status ready={a.ready} label={a.reason} />
                </div>
                <p dir="ltr" className="mt-1 truncate text-start font-mono text-subtle text-xs">
                  {a.image ?? a.git_repo}
                </p>
                {a.url && (
                  <p dir="ltr" className="mt-1 truncate text-start text-link text-xs">
                    {a.url}
                  </p>
                )}
                {!a.ready && a.message && (
                  <p dir="auto" className="mt-2 text-warn/80 text-xs">
                    {a.message}
                  </p>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}

      <Templates project={project} environment={environment} />

      <Secrets project={project} environment={environment} />

      <RegistryLogins project={project} environment={environment} />

      <DetachedCard project={project} environment={environment} />

      <div className="border-line border-t pt-6">
        <ConfirmDelete
          name={env.name}
          what={t('environment.what')}
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
  const { t } = usePrefs()
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
      className="grid gap-4 rounded-xl border border-line bg-surface p-4 sm:grid-cols-3"
    >
      <TextField
        label={t('projects.name')}
        name="name"
        required
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={40}
        placeholder="api"
      />
      <div className="sm:col-span-2">
        <TextField
          label={t('environment.image')}
          name="image"
          required
          placeholder="ghcr.io/acme/api:1.4.2"
        />
      </div>
      <TextField
        label={t('environment.port')}
        name="port"
        type="number"
        min={1}
        max={65535}
        placeholder={t('environment.portPlaceholder')}
      />
      <TextField
        label={t('environment.replicas')}
        name="replicas"
        type="number"
        min={0}
        max={50}
        defaultValue={1}
      />
      <TextField
        label={t('environment.autoscale')}
        name="max_replicas"
        type="number"
        min={1}
        max={50}
        placeholder={t('environment.optional')}
      />
      <Select label={t('environment.size')} name="size" defaultValue="small">
        <option value="nano">nano — 50m / 128Mi</option>
        <option value="small">small — 100m / 256Mi</option>
        <option value="medium">medium — 250m / 1Gi</option>
        <option value="large">large — 1 CPU / 4Gi</option>
      </Select>
      <TextField label={t('environment.healthPath')} name="health" placeholder="/healthz" />
      <TextField label={t('environment.domains')} name="domains" placeholder="api.example.com" />
      <Select label={t('environment.protocol')} name="protocol" defaultValue="http">
        <option value="http">http — {t('environment.protocolHttp')}</option>
        <option value="tcp">tcp — {t('environment.protocolTcp')}</option>
      </Select>
      <TextField
        label={t('environment.schedule')}
        name="schedule"
        placeholder={t('environment.schedulePlaceholder')}
      />
      <TextField
        label={t('environment.volume')}
        name="volume"
        placeholder={t('environment.volumePlaceholder')}
        pattern="/[^:]+(:[0-9]+(Ki|Mi|Gi|Ti))?"
      />
      <div className="sm:col-span-3">
        <TextArea
          label={t('environment.envVars')}
          name="env"
          placeholder={'LOG_LEVEL=info\nDATABASE_URL=@db/url'}
          hint={t('environment.envVarsHint')}
        />
      </div>
      <div className="flex items-center gap-3 sm:col-span-3">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? t('ui.deploying') : t('ui.deploy')}
        </Button>
        {envError && <ErrorNote error={new Error(envError)} />}
        <ErrorNote error={mutation.error} />
      </div>
    </form>
  )
}

function Secrets({ project, environment }: { project: string; environment: string }) {
  const { t } = usePrefs()
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
      setFormError(errors.length ? errors.join('; ') : t('environment.secretNeedsKey'))
      return
    }
    setFormError(null)
    save.mutate(
      { name: String(form.get('name') ?? '').trim(), data },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <Card title={t('environment.secrets')}>
      <div className="space-y-4">
        {secrets.isError ? (
          <ErrorNote error={secrets.error} />
        ) : secrets.data?.length ? (
          <ul className="divide-y divide-line-soft">
            {secrets.data.map((s) => (
              <li key={s.name} className="flex items-center justify-between gap-3 py-2">
                <div className="min-w-0">
                  <span dir="ltr" className="font-mono text-sm">
                    {s.name}
                  </span>
                  <p className="truncate text-subtle text-xs">
                    <span dir="ltr">{s.keys.join(', ')}</span>
                    {s.revision != null && ` · ${fill(t('environment.revision'), { revision: s.revision })}`}
                    {s.storage === 'cluster' && ` · ${t('environment.storedInCluster')}`}
                    {s.revoked && ` · ${t('environment.secretRevoked')}`}
                  </p>
                </div>
                <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate(s.name)}>
                  {t('ui.remove')}
                </Button>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-subtle text-sm">{t('environment.noSecrets')}</p>
        )}
        <form onSubmit={onSubmit} className="grid gap-3 sm:grid-cols-3">
          <TextField
            label={t('environment.secretName')}
            name="name"
            required
            pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
            placeholder="db"
          />
          <div className="sm:col-span-2">
            <TextArea
              label={t('environment.keys')}
              name="data"
              placeholder="url=postgres://…"
              hint={t('environment.keysHint')}
            />
          </div>
          <div className="flex items-center gap-3 sm:col-span-3">
            <Button type="submit" variant="secondary" disabled={save.isPending}>
              {save.isPending ? t('ui.saving') : t('environment.saveSecret')}
            </Button>
            {formError && <ErrorNote error={new Error(formError)} />}
            <ErrorNote error={save.error ?? remove.error} />
          </div>
        </form>
      </div>
    </Card>
  )
}

function RegistryLogins({ project, environment }: { project: string; environment: string }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const logins = useQuery({ ...registriesQuery(project, environment), retry: false })
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['registries', project, environment] })
  const save = useMutation({
    mutationFn: ({
      name,
      ...login
    }: {
      name: string
      registry: string
      username: string
      password: string
    }) => putRegistryLogin(project, environment, name, login),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: (name: string) => deleteRegistryLogin(project, environment, name),
    onSuccess: refresh,
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const field = (key: string) => String(form.get(key) ?? '').trim()
    save.mutate(
      {
        name: field('name'),
        registry: field('registry'),
        username: field('username'),
        password: field('password'),
      },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <Card title={t('environment.registryLogins')}>
      <div className="space-y-4">
        {logins.isError ? (
          <ErrorNote error={logins.error} />
        ) : logins.data?.length ? (
          <ul className="divide-y divide-line-soft">
            {logins.data.map((l) => (
              <li key={l.name} className="flex items-center justify-between gap-3 py-2">
                <div className="min-w-0">
                  <span dir="ltr" className="font-mono text-sm">
                    {l.registry}
                  </span>
                  <p className="truncate text-subtle text-xs">
                    {l.name} · {fill(t('environment.revision'), { revision: l.revision })}
                    {l.revoked && ` · ${t('environment.loginRevoked')}`}
                  </p>
                </div>
                <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate(l.name)}>
                  {t('ui.remove')}
                </Button>
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-subtle text-sm">{t('environment.noLogins')}</p>
        )}
        <form onSubmit={onSubmit} className="grid gap-3 sm:grid-cols-2">
          <TextField
            label={t('projects.name')}
            name="name"
            required
            pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
            placeholder="ghcr"
          />
          <TextField label={t('environment.registry')} name="registry" required placeholder="ghcr.io" />
          <TextField label={t('environment.username')} name="username" required autoComplete="off" />
          <TextField
            label={t('environment.passwordOrToken')}
            name="password"
            type="password"
            required
            autoComplete="new-password"
          />
          <div className="flex items-center gap-3 sm:col-span-2">
            <Button type="submit" variant="secondary" disabled={save.isPending}>
              {save.isPending ? t('ui.saving') : t('environment.saveLogin')}
            </Button>
            <ErrorNote error={save.error ?? remove.error} />
          </div>
        </form>
      </div>
    </Card>
  )
}

function Templates({ project, environment }: { project: string; environment: string }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const templates = useQuery(templatesQuery)
  const [chosen, setChosen] = useState<string | null>(null)
  const [deployed, setDeployed] = useState<DeployedTemplate | null>(null)
  const deploy = useMutation({
    mutationFn: ({ id, name }: { id: string; name: string }) =>
      deployTemplate(project, environment, id, name),
    onSuccess: async (result) => {
      setDeployed(result)
      setChosen(null)
      await queryClient.invalidateQueries({ queryKey: ['apps', project, environment] })
      await queryClient.invalidateQueries({ queryKey: ['secrets', project, environment] })
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>, id: string) {
    event.preventDefault()
    deploy.mutate({ id, name: String(new FormData(event.currentTarget).get('name') ?? '').trim() })
  }

  return (
    <Card title={t('environment.templates')}>
      {deployed && (
        <div role="status" className="mb-4 rounded-lg border border-ok/30 bg-ok/5 p-3 text-sm">
          {t('environment.templateDeployed')} <strong dir="auto">{deployed.app.name}</strong>.{' '}
          {t('environment.credentialsIn')}{' '}
          <code dir="ltr" className="font-mono">
            {deployed.credentials_secret}
          </code>
          {deployed.connection_keys.includes('url') && (
            <>
              {' '}
              — {t('environment.connectWith')}{' '}
              <code dir="ltr" className="font-mono">
                DATABASE_URL=@{deployed.credentials_secret}/url
              </code>
            </>
          )}
          .
        </div>
      )}
      <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {templates.data?.map((tpl) => (
          <li key={tpl.id} className="flex flex-col gap-2 rounded-lg border border-line p-3">
            <div className="flex items-center justify-between gap-2">
              <span className="font-medium text-sm">{tpl.name}</span>
              <Badge>{tpl.protocol}</Badge>
            </div>
            <p dir="auto" className="flex-1 text-muted-foreground text-xs">
              {tpl.description}
            </p>
            <p dir="ltr" className="truncate text-start font-mono text-subtle text-xs">
              {tpl.image}
            </p>
            {chosen === tpl.id ? (
              <form onSubmit={(e) => onSubmit(e, tpl.id)} className="space-y-2">
                <TextField
                  label={t('environment.appName')}
                  name="name"
                  required
                  defaultValue={tpl.category === 'database' ? 'db' : tpl.id}
                  pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
                  maxLength={40}
                />
                <div className="flex gap-2">
                  <Button type="submit" disabled={deploy.isPending}>
                    {deploy.isPending ? t('ui.deploying') : t('ui.deploy')}
                  </Button>
                  <Button variant="ghost" onClick={() => setChosen(null)}>
                    {t('ui.cancel')}
                  </Button>
                </div>
              </form>
            ) : (
              <Button variant="secondary" onClick={() => setChosen(tpl.id)}>
                {t('environment.useTemplate')}
              </Button>
            )}
          </li>
        ))}
      </ul>
      <ErrorNote error={deploy.error ?? templates.error} />
    </Card>
  )
}
