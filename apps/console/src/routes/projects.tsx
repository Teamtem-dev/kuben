import { useMutation, useQueryClient } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { PlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import { EmptyState, ErrorAlert, FormDialog, linkCard, StatusBadge, TextInput } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { createProject, type Project, projectQuery } from '@/lib/api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'

/** The "New project" button and its dialog. */
export function NewProjectButton() {
  const { t } = usePrefs()
  const [creating, setCreating] = useState(false)
  return (
    <FormDialog
      open={creating}
      onOpenChange={setCreating}
      title={t('projects.new')}
      trigger={
        <Button>
          <PlusIcon aria-hidden="true" />
          {t('projects.new')}
        </Button>
      }
    >
      <CreateProjectForm onDone={() => setCreating(false)} />
    </FormDialog>
  )
}

/** The projects as a grid of cards, each a link to its page. */
export function ProjectCards({ projects }: { projects: readonly Project[] }) {
  const { t } = usePrefs()
  if (projects.length === 0) return <EmptyState>{t('projects.empty')}</EmptyState>
  return (
    <ul className="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
      {projects.map((p) => (
        <li key={p.name}>
          <Link to="/projects/$project" params={{ project: p.name }} className={linkCard}>
            <div className="flex items-center justify-between gap-3">
              <span dir="auto" className="truncate font-medium">
                {p.display_name}
              </span>
              <StatusBadge ready={p.ready} label={p.deleting ? t('projects.deleting') : undefined} />
            </div>
            <p className="mt-1 text-muted-foreground text-xs">
              <span dir="ltr" className="font-mono">
                {p.name}
              </span>{' '}
              ·{' '}
              {p.environments === 1
                ? t('projects.environmentsOne')
                : fill(t('projects.environments'), { count: p.environments })}
            </p>
            {p.description && (
              <p dir="auto" className="mt-2 line-clamp-2 text-muted-foreground text-sm">
                {p.description}
              </p>
            )}
          </Link>
        </li>
      ))}
    </ul>
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
    <form onSubmit={onSubmit} className="grid gap-4">
      <TextInput
        label={t('projects.name')}
        name="name"
        required
        dir="ltr"
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={40}
        placeholder="shop"
        hint={t('projects.nameHint')}
      />
      <TextInput
        label={t('projects.displayName')}
        name="display_name"
        required
        maxLength={100}
        placeholder="Online Shop"
      />
      <TextInput label={t('projects.description')} name="description" placeholder={t('projects.optional')} />
      <ErrorAlert error={mutation.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={mutation.isPending}>
          {mutation.isPending ? t('ui.creating') : t('projects.create')}
        </Button>
      </DialogFooter>
    </form>
  )
}
