import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { PlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  ConfirmDelete,
  EmptyState,
  ErrorAlert,
  FormDialog,
  linkCard,
  PageHeader,
  Section,
  SelectInput,
  StatusBadge,
  Tag,
  TextInput,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { createEnvironment, deleteProject, type EnvType, environmentsQuery, projectQuery } from '@/lib/api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { OwnerCard } from './ops/controls'
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
    <div className="space-y-6">
      <PageHeader
        title={<span dir="auto">{p.display_name}</span>}
        description={<span dir="auto">{p.description ?? p.name}</span>}
        actions={
          <FormDialog
            open={creating}
            onOpenChange={setCreating}
            title={t('project.newEnvironment')}
            className="sm:max-w-2xl"
            trigger={
              <Button>
                <PlusIcon aria-hidden="true" />
                {t('project.newEnvironment')}
              </Button>
            }
          >
            <CreateEnvironmentForm project={project} onDone={() => setCreating(false)} />
          </FormDialog>
        }
      />

      {environments.length === 0 ? (
        <EmptyState>{t('project.empty')}</EmptyState>
      ) : (
        <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
          {environments.map((e) => (
            <li key={e.resource_name}>
              <Link
                to="/projects/$project/$environment"
                params={{ project, environment: e.name }}
                className={linkCard}
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="truncate font-medium">{e.name}</span>
                  <StatusBadge
                    ready={e.ready}
                    label={e.deleting ? t('project.terminating') : (e.phase ?? undefined)}
                  />
                </div>
                <div className="mt-2 flex items-center gap-2">
                  <Tag>{e.env_type}</Tag>
                  <span dir="ltr" className="truncate font-mono text-muted-foreground text-xs">
                    {e.namespace}
                  </span>
                </div>
                {e.message && (
                  <p dir="auto" className="mt-2 text-warning text-xs">
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

      <OwnerCard project={project} />

      <Section
        tone="danger"
        title={t('ui.dangerZone')}
        description={environments.length > 0 ? t('project.deleteEnvironmentsFirst') : undefined}
        actions={
          <ConfirmDelete
            name={project}
            what={t('project.what')}
            pending={remove.isPending}
            error={remove.error}
            disabled={environments.length > 0}
            onConfirm={() => remove.mutate()}
          />
        }
      />
    </div>
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
    <form onSubmit={onSubmit} className="grid gap-4 sm:grid-cols-3">
      <TextInput
        label={t('projects.name')}
        name="name"
        required
        dir="ltr"
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={20}
        placeholder="staging"
      />
      <SelectInput
        label={t('project.type')}
        name="env_type"
        defaultValue="standard"
        hint={t('project.typeHint')}
        className="sm:col-span-2"
      >
        <option value="standard">{t('project.type.standard')}</option>
        <option value="production">{t('project.type.production')}</option>
        <option value="preview">{t('project.type.preview')}</option>
      </SelectInput>
      <TextInput
        label={t('project.cpuQuota')}
        name="cpu"
        placeholder={fill(t('ui.example'), { value: '4' })}
      />
      <TextInput
        label={t('project.memoryQuota')}
        name="memory"
        placeholder={fill(t('ui.example'), { value: '8Gi' })}
      />
      <TextInput
        label={t('project.maxPods')}
        name="pods"
        type="number"
        min={1}
        placeholder={fill(t('ui.example'), { value: '50' })}
      />
      <ErrorAlert error={mutation.error} className="sm:col-span-3" />
      <DialogFooter className="sm:col-span-3">
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? t('ui.creating') : t('project.createEnvironment')}
        </Button>
      </DialogFooter>
    </form>
  )
}
