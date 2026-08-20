import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Empty, ErrorNote, PageHeader, Status, TextField } from '../components/ui'
import { createProject, projectQuery, projectsQuery } from '../lib/api'

export function ProjectsPage() {
  const { data: projects } = useSuspenseQuery(projectsQuery)
  const [creating, setCreating] = useState(false)

  return (
    <section className="space-y-6">
      <PageHeader
        title="Projects"
        subtitle={projects.length === 1 ? '1 project' : `${projects.length} projects`}
        actions={
          <Button variant={creating ? 'secondary' : 'primary'} onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New project'}
          </Button>
        }
      />

      {creating && <CreateProjectForm onDone={() => setCreating(false)} />}

      {projects.length === 0 ? (
        <Empty>No projects yet. A project groups environments such as staging and production.</Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {projects.map((p) => (
            <li key={p.name}>
              <Link
                to="/projects/$project"
                params={{ project: p.name }}
                className="block rounded-xl border border-white/10 bg-slate-900/50 p-4 transition hover:border-sky-400/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="truncate font-medium">{p.display_name}</span>
                  <Status ready={p.ready} label={p.deleting ? 'Deleting' : undefined} />
                </div>
                <p className="mt-1 font-mono text-slate-500 text-xs">
                  {p.name} · {p.environments === 1 ? '1 environment' : `${p.environments} environments`}
                </p>
                {p.description && <p className="mt-2 line-clamp-2 text-slate-400 text-sm">{p.description}</p>}
              </Link>
            </li>
          ))}
        </ul>
      )}
    </section>
  )
}

function CreateProjectForm({ onDone }: { onDone: () => void }) {
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
      className="grid gap-4 rounded-xl border border-white/10 bg-slate-900/50 p-4 sm:grid-cols-2"
    >
      <TextField
        label="Name"
        name="name"
        required
        pattern="[a-z0-9]([-a-z0-9]*[a-z0-9])?"
        maxLength={40}
