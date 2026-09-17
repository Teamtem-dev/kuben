import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Empty, ErrorNote, PageHeader, Status, TextField } from '../components/ui'
import { createProject, projectQuery, projectsQuery } from '../lib/api'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'

export function ProjectsPage() {
  const { t } = usePrefs()
  const { data: projects } = useSuspenseQuery(projectsQuery)
  const [creating, setCreating] = useState(false)

  return (
    <section className="space-y-6">
      <PageHeader
        title={t('projects.title')}
        subtitle={
          projects.length === 1
            ? t('projects.countOne')
            : fill(t('projects.count'), { count: projects.length })
        }
        actions={
          <Button variant={creating ? 'secondary' : 'primary'} onClick={() => setCreating((v) => !v)}>
            {creating ? t('ui.cancel') : t('projects.new')}
          </Button>
        }
      />

      {creating && <CreateProjectForm onDone={() => setCreating(false)} />}

      {projects.length === 0 ? (
        <Empty>{t('projects.empty')}</Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects.map((p) => (
            <li key={p.name}>
              <Link
                to="/projects/$project"
                params={{ project: p.name }}
                className="block rounded-xl border border-line bg-surface p-4 transition hover:border-accent/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span dir="auto" className="truncate font-medium">
                    {p.display_name}
                  </span>
                  <Status ready={p.ready} label={p.deleting ? t('projects.deleting') : undefined} />
                </div>
                <p className="mt-1 font-mono text-subtle text-xs">
                  {p.name} ·{' '}
                  {p.environments === 1
                    ? t('projects.environmentsOne')
                    : fill(t('projects.environments'), { count: p.environments })}
                </p>
                {p.description && (
                  <p dir="auto" className="mt-2 line-clamp-2 text-muted text-sm">
                    {p.description}
                  </p>
                )}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function CreateProjectForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const mutation = useMutation({
    mutationFn: createProject,
    onSuccess: async (project) => {
      // Seed the detail query: the projection may lag the create by a few ms.
      queryClient.setQueryData(projectQuery(project.name).queryKey, project)
      await queryClient.invalidateQueries({ queryKey: ['projects'], exact: true })
      onDone()
      await navigate({ to: '/projects/$project', params: { project: project.name } })
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const description = String(form.get('description') ?? '').trim()
    mutation.mutate({
      name: String(form.get('name') ?? '').trim(),
      display_name: String(form.get('display_name') ?? '').trim(),
      description: description || null,
    })
  }

  return (
    <form
      onSubmit={onSubmit}
      className="grid gap-4 rounded-xl border border-line bg-surface p-4 sm:grid-cols-2"
    >
      <TextField
        label={t('projects.name')}
        name="name"
        required
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={40}
        placeholder="shop"
        hint={t('projects.nameHint')}
      />
      <TextField
        label={t('projects.displayName')}
        name="display_name"
        required
        maxLength={100}
        placeholder="Online Shop"
      />
      <div className="sm:col-span-2">
        <TextField
          label={t('projects.description')}
          name="description"
          placeholder={t('projects.optional')}
        />
      </div>
      <div className="flex items-center gap-3 sm:col-span-2">
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? t('ui.creating') : t('projects.create')}
        </Button>
        <ErrorNote error={mutation.error} />
      </div>
    </form>
  )
}
