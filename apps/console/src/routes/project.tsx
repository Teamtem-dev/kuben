import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  ConfirmDelete,
  Empty,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextField,
} from '../components/ui'
import { createEnvironment, deleteProject, type EnvType, environmentsQuery, projectQuery } from '../lib/api'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'
import { PreviewsCard, StatusPageCard } from './ops/project-ops'

const route = getRouteApi('/_authed/projects/$project')

export function ProjectPage() {
  const { project } = route.useParams()
  const { t } = usePrefs()
  const { data: p } = useSuspenseQuery(projectQuery(project))
  const { data: environments } = useSuspenseQuery(environmentsQuery(project))
  const [creating, setCreating] = useState(false)
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const remove = useMutation({
    mutationFn: () => deleteProject(project),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['projects'] })
      await navigate({ to: '/' })
    },
  })

  return (
    <section className="space-y-6">
      <PageHeader
        crumbs={
          <Link to="/" className="hover:text-fg">
            {t('projects.title')}
          </Link>
        }
        title={<span dir="auto">{p.display_name}</span>}
        subtitle={<span dir="auto">{p.description ?? p.name}</span>}
        actions={
          <Button variant={creating ? 'secondary' : 'primary'} onClick={() => setCreating((v) => !v)}>
            {creating ? t('ui.cancel') : t('project.newEnvironment')}
          </Button>
        }
      />

      {creating && <CreateEnvironmentForm project={project} onDone={() => setCreating(false)} />}

      {environments.length === 0 ? (
        <Empty>{t('project.empty')}</Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {environments.map((e) => (
            <li key={e.resource_name}>
              <Link
                to="/projects/$project/$environment"
                params={{ project, environment: e.name }}
                className="block rounded-xl border border-line bg-surface p-4 transition hover:border-brand/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="truncate font-medium">{e.name}</span>
                  <Status
                    ready={e.ready}
                    label={e.deleting ? t('project.terminating') : (e.phase ?? undefined)}
                  />
                </div>
                <div className="mt-2 flex items-center gap-2">
                  <Badge>{e.env_type}</Badge>
                  <span dir="ltr" className="truncate font-mono text-subtle text-xs">
                    {e.namespace}
                  </span>
                </div>
                {e.message && (
                  <p dir="auto" className="mt-2 text-warn/80 text-xs">
                    {e.message}
                  </p>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}

      <PreviewsCard project={project} />

      <StatusPageCard project={project} />

      <div className="border-line border-t pt-6">
        {environments.length > 0 ? (
          <p className="text-subtle text-sm">{t('project.deleteEnvironmentsFirst')}</p>
        ) : (
          <ConfirmDelete
            name={project}
            what={t('project.what')}
            pending={remove.isPending}
            error={remove.error}
            onConfirm={() => remove.mutate()}
          />
        )}
      </div>
    </section>
  )
}

function CreateEnvironmentForm({ project, onDone }: { project: string; onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const mutation = useMutation({
    mutationFn: (body: Parameters<typeof createEnvironment>[1]) => createEnvironment(project, body),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['environments', project] })
      onDone()
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const text = (key: string) => String(form.get(key) ?? '').trim() || null
    const pods = text('pods')
    const quota = { cpu: text('cpu'), memory: text('memory'), pods: pods ? Number(pods) : null }
    mutation.mutate({
      name: String(form.get('name') ?? '').trim(),
      env_type: String(form.get('env_type')) as EnvType,
      quota: quota.cpu || quota.memory || quota.pods ? quota : null,
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
        maxLength={20}
        placeholder="staging"
      />
      <Select label={t('project.type')} name="env_type" defaultValue="standard" hint={t('project.typeHint')}>
        <option value="standard">{t('project.type.standard')}</option>
        <option value="production">{t('project.type.production')}</option>
        <option value="preview">{t('project.type.preview')}</option>
      </Select>
      <div />
      <TextField
        label={t('project.cpuQuota')}
        name="cpu"
        placeholder={fill(t('ui.example'), { value: '4' })}
      />
      <TextField
        label={t('project.memoryQuota')}
        name="memory"
        placeholder={fill(t('ui.example'), { value: '8Gi' })}
      />
      <TextField
        label={t('project.maxPods')}
        name="pods"
        type="number"
        min={1}
        placeholder={fill(t('ui.example'), { value: '50' })}
      />
      <div className="flex items-center gap-3 sm:col-span-3">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? t('ui.creating') : t('project.createEnvironment')}
        </Button>
        <ErrorNote error={mutation.error} />
      </div>
    </form>
  )
}
