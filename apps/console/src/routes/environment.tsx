import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { PlusIcon, Trash2Icon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  ConfirmDelete,
  EmptyState,
  ErrorAlert,
  Loading,
  linkCard,
  Notice,
  PageHeader,
  Section,
  SelectInput,
  StatusBadge,
  Tag,
  TextareaInput,
  TextInput,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import {
  appsQuery,
  createApp,
  type DeployedTemplate,
  deleteEnvironment,
  deleteRegistryLogin,
  deleteSecret,
  deployTemplate,
  environmentQuery,
  putRegistryLogin,
  putSecret,
  registriesQuery,
  secretsQuery,
  templatesQuery,
} from '@/lib/api'
import { parseEnvLines } from '@/lib/env'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { WindowsCard } from './ops/controls'
import { DetachedCard } from './ops/environment-ops'

const route = getRouteApi('/_authed/projects/$project/$environment')

export function EnvironmentPage() {
  const { project, environment } = route.useParams()
  const { t, locale } = usePrefs()
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
    <div className="space-y-6">
      <PageHeader
        title={
          <>
            {env.name} <Tag>{env.env_type}</Tag>
          </>
        }
        description={
          <span dir="ltr" className="font-mono">
            {env.namespace}
          </span>
        }
        actions={
          <Button
            variant={deploying ? 'outline' : 'default'}
            onClick={() => setDeploying((v) => !v)}
            disabled={env.deleting}
            aria-expanded={deploying}
          >
            {!deploying && <PlusIcon aria-hidden="true" />}
            {deploying ? t('ui.cancel') : t('environment.deployApp')}
          </Button>
        }
      />

      {env.deleting && (
        <Notice tone="warning">
          {t('environment.deleting')}
          {env.deletion_scheduled_at
            ? ` — ${fill(t('environment.purgedAt'), { date: new Date(env.deletion_scheduled_at).toLocaleString(locale) })}`
            : ''}
          .
        </Notice>
      )}

      {deploying && (
        <DeployForm project={project} environment={environment} onDone={() => setDeploying(false)} />
      )}

      {apps.length === 0 ? (
        <EmptyState>{t('environment.empty')}</EmptyState>
      ) : (
        <ul className="grid gap-4 sm:grid-cols-2">
          {apps.map((a) => (
            <li key={a.name}>
              <Link
                to="/projects/$project/$environment/$app"
                params={{ project, environment, app: a.name }}
                className={linkCard}
              >
                <div className="flex items-center justify-between gap-3">
                  <span dir="auto" className="truncate font-medium">
                    {a.name}
                  </span>
                  <StatusBadge ready={a.ready} label={a.reason} />
                </div>
                <p dir="ltr" className="mt-1 truncate text-start font-mono text-muted-foreground text-xs">
                  {a.image ?? a.git_repo}
                </p>
                {a.url && (
                  <p dir="ltr" className="mt-1 truncate text-start text-link text-xs">
                    {a.url}
                  </p>
                )}
                {!a.ready && a.message && (
                  <p dir="auto" className="mt-2 text-warning text-xs">
                    {a.message}
                  </p>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}

      <Templates project={project} environment={environment} />

      <div className="grid gap-6 xl:grid-cols-2">
        <Secrets project={project} environment={environment} />
        <RegistryLogins project={project} environment={environment} />
      </div>

      <div className="grid gap-6 xl:grid-cols-2">
        <WindowsCard kind="freeze" project={project} environment={environment} />
        <WindowsCard
          kind="silence"
          project={project}
          environment={environment}
          apps={apps.map((a) => a.name)}
        />
      </div>

      <DetachedCard project={project} environment={environment} />

      <Section
        tone="danger"
        title={t('ui.dangerZone')}
        actions={
          <ConfirmDelete
            name={env.name}
            what={t('environment.what')}
            pending={remove.isPending}
            error={remove.error}
            onConfirm={() => remove.mutate()}
          />
        }
      />
    </div>
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
    <Section title={t('environment.deployApp')}>
      <form onSubmit={onSubmit} className="grid gap-4 sm:grid-cols-3">
        <TextInput
          label={t('projects.name')}
          name="name"
          required
          dir="ltr"
          pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
          maxLength={40}
          placeholder="api"
        />
        <TextInput
          label={t('environment.image')}
          name="image"
          required
          dir="ltr"
          placeholder="ghcr.io/acme/api:1.4.2"
          className="sm:col-span-2"
        />
        <TextInput
          label={t('environment.port')}
          name="port"
          type="number"
          min={1}
          max={65535}
          placeholder={t('environment.portPlaceholder')}
        />
        <TextInput
          label={t('environment.replicas')}
          name="replicas"
          type="number"
          min={0}
          max={50}
          defaultValue={1}
        />
        <TextInput
          label={t('environment.autoscale')}
          name="max_replicas"
          type="number"
          min={1}
          max={50}
          placeholder={t('environment.optional')}
        />
        <SelectInput label={t('environment.size')} name="size" defaultValue="small">
          <option value="nano">nano — 50m / 128Mi</option>
          <option value="small">small — 100m / 256Mi</option>
          <option value="medium">medium — 250m / 1Gi</option>
          <option value="large">large — 1 CPU / 4Gi</option>
        </SelectInput>
        <TextInput label={t('environment.healthPath')} name="health" dir="ltr" placeholder="/healthz" />
        <TextInput label={t('environment.domains')} name="domains" dir="ltr" placeholder="api.example.com" />
        <SelectInput label={t('environment.protocol')} name="protocol" defaultValue="http">
          <option value="http">http — {t('environment.protocolHttp')}</option>
          <option value="tcp">tcp — {t('environment.protocolTcp')}</option>
        </SelectInput>
        <TextInput
          label={t('environment.schedule')}
          name="schedule"
          placeholder={t('environment.schedulePlaceholder')}
        />
        <TextInput
          label={t('environment.volume')}
          name="volume"
          placeholder={t('environment.volumePlaceholder')}
          pattern="/[^:]+(:[0-9]+(Ki|Mi|Gi|Ti))?"
        />
        <TextareaInput
          label={t('environment.envVars')}
          name="env"
          dir="ltr"
          placeholder={'LOG_LEVEL=info\nDATABASE_URL=@db/url'}
          hint={t('environment.envVarsHint')}
          className="sm:col-span-3"
        />
        {envError && <ErrorAlert error={new Error(envError)} className="sm:col-span-3" />}
        <ErrorAlert error={mutation.error} className="sm:col-span-3" />
        <div className="flex gap-2 sm:col-span-3">
          <Button type="submit" disabled={mutation.isPending}>
            {mutation.isPending ? t('ui.deploying') : t('ui.deploy')}
          </Button>
          <Button type="button" variant="outline" onClick={onDone}>
            {t('ui.cancel')}
          </Button>
        </div>
      </form>
    </Section>
  )
}

/** One stored item (a secret, a registry login) with its remove button. */
function StoredItem({
  title,
  detail,
  removing,
  onRemove,
}: {
  title: string
  detail: string
  removing: boolean
  onRemove: () => void
}) {
  const { t } = usePrefs()
  return (
    <Item role="listitem" variant="outline" size="sm">
      <ItemContent className="min-w-0">
        <ItemTitle dir="ltr" className="font-mono">
          {title}
        </ItemTitle>
        <ItemDescription className="truncate">{detail}</ItemDescription>
      </ItemContent>
      <ItemActions>
        <Button
          variant="ghost"
          size="sm"
          className="text-destructive hover:text-destructive"
          disabled={removing}
          onClick={onRemove}
        >
          <Trash2Icon aria-hidden="true" />
          {t('ui.remove')}
        </Button>
      </ItemActions>
    </Item>
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
    <Section title={t('environment.secrets')}>
      {secrets.isError ? (
        <ErrorAlert error={secrets.error} />
      ) : secrets.isPending ? (
        <Loading lines={2} />
      ) : secrets.data?.length ? (
        <ItemGroup className="gap-2">
          {secrets.data.map((s) => (
            <StoredItem
              key={s.name}
              title={s.name}
              detail={[
                s.keys.join(', '),
                s.revision != null ? fill(t('environment.revision'), { revision: s.revision }) : null,
                s.storage === 'cluster' ? t('environment.storedInCluster') : null,
                s.revoked ? t('environment.secretRevoked') : null,
              ]
                .filter(Boolean)
                .join(' · ')}
              removing={remove.isPending}
              onRemove={() => remove.mutate(s.name)}
            />
          ))}
        </ItemGroup>
      ) : (
        <p className="text-muted-foreground text-sm">{t('environment.noSecrets')}</p>
      )}
      <form onSubmit={onSubmit} className="grid gap-4">
        <TextInput
          label={t('environment.secretName')}
          name="name"
          required
          dir="ltr"
          pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
          placeholder="db"
        />
        <TextareaInput
          label={t('environment.keys')}
          name="data"
          dir="ltr"
          placeholder="url=postgres://…"
          hint={t('environment.keysHint')}
        />
        {formError && <ErrorAlert error={new Error(formError)} />}
        <ErrorAlert error={save.error ?? remove.error} />
        <div>
          <Button type="submit" variant="secondary" disabled={save.isPending}>
            {save.isPending ? t('ui.saving') : t('environment.saveSecret')}
          </Button>
        </div>
      </form>
    </Section>
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
    <Section title={t('environment.registryLogins')}>
      {logins.isError ? (
        <ErrorAlert error={logins.error} />
      ) : logins.isPending ? (
        <Loading lines={2} />
      ) : logins.data?.length ? (
        <ItemGroup className="gap-2">
          {logins.data.map((l) => (
            <StoredItem
              key={l.name}
              title={l.registry}
              detail={[
                l.name,
                fill(t('environment.revision'), { revision: l.revision }),
                l.revoked ? t('environment.loginRevoked') : null,
              ]
                .filter(Boolean)
                .join(' · ')}
              removing={remove.isPending}
              onRemove={() => remove.mutate(l.name)}
            />
          ))}
        </ItemGroup>
      ) : (
        <p className="text-muted-foreground text-sm">{t('environment.noLogins')}</p>
      )}
      <form onSubmit={onSubmit} className="grid gap-4 sm:grid-cols-2">
        <TextInput
          label={t('projects.name')}
          name="name"
          required
          dir="ltr"
          pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
          placeholder="ghcr"
        />
        <TextInput
          label={t('environment.registry')}
          name="registry"
          required
          dir="ltr"
          placeholder="ghcr.io"
        />
        <TextInput label={t('environment.username')} name="username" required dir="ltr" autoComplete="off" />
        <TextInput
          label={t('environment.passwordOrToken')}
          name="password"
          type="password"
          required
          dir="ltr"
          autoComplete="new-password"
        />
        <ErrorAlert error={save.error ?? remove.error} className="sm:col-span-2" />
        <div className="sm:col-span-2">
          <Button type="submit" variant="secondary" disabled={save.isPending}>
            {save.isPending ? t('ui.saving') : t('environment.saveLogin')}
          </Button>
        </div>
      </form>
    </Section>
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
    <Section title={t('environment.templates')}>
      {deployed && (
        <Notice tone="success">
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
        </Notice>
      )}
      {templates.isPending && <Loading lines={2} />}
      <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        {templates.data?.map((tpl) => (
          <li key={tpl.id} className="flex flex-col gap-2 rounded-lg border p-3">
            <div className="flex items-center justify-between gap-2">
              <span className="font-medium text-sm">{tpl.name}</span>
              <Tag>{tpl.protocol}</Tag>
            </div>
            <p dir="auto" className="flex-1 text-muted-foreground text-xs">
              {tpl.description}
            </p>
            <p dir="ltr" className="truncate text-start font-mono text-muted-foreground text-xs">
              {tpl.image}
            </p>
            {chosen === tpl.id ? (
              <form onSubmit={(e) => onSubmit(e, tpl.id)} className="grid gap-2">
                <TextInput
                  label={t('environment.appName')}
                  name="name"
                  required
                  dir="ltr"
                  defaultValue={tpl.category === 'database' ? 'db' : tpl.id}
                  pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
                  maxLength={40}
                />
                <div className="flex gap-2">
                  <Button type="submit" size="sm" disabled={deploy.isPending}>
                    {deploy.isPending ? t('ui.deploying') : t('ui.deploy')}
                  </Button>
                  <Button type="button" size="sm" variant="ghost" onClick={() => setChosen(null)}>
                    {t('ui.cancel')}
                  </Button>
                </div>
              </form>
            ) : (
              <Button variant="outline" size="sm" onClick={() => setChosen(tpl.id)}>
                {t('environment.useTemplate')}
              </Button>
            )}
          </li>
        ))}
      </ul>
      <ErrorAlert error={deploy.error ?? templates.error} />
    </Section>
  )
}
