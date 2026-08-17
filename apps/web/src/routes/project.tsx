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

const route = getRouteApi('/_authed/projects/$project')

export function ProjectPage() {
  const { project } = route.useParams()
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
          <Link to="/" className="hover:text-slate-200">
            Projects
          </Link>
        }
        title={p.display_name}
        subtitle={p.description ?? p.name}
        actions={
          <Button variant={creating ? 'secondary' : 'primary'} onClick={() => setCreating((v) => !v)}>
            {creating ? 'Cancel' : 'New environment'}
          </Button>
        }
      />

      {creating && <CreateEnvironmentForm project={project} onDone={() => setCreating(false)} />}

      {environments.length === 0 ? (
        <Empty>No environments yet. Each environment gets its own isolated namespace.</Empty>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {environments.map((e) => (
            <li key={e.resource_name}>
              <Link
                to="/projects/$project/$environment"
                params={{ project, environment: e.name }}
                className="block rounded-xl border border-white/10 bg-slate-900/50 p-4 transition hover:border-sky-400/40"
              >
                <div className="flex items-center justify-between gap-3">
                  <span className="truncate font-medium">{e.name}</span>
                  <Status ready={e.ready} label={e.deleting ? 'Terminating' : (e.phase ?? undefined)} />
                </div>
                <div className="mt-2 flex items-center gap-2">
                  <Badge>{e.env_type}</Badge>
                  <span className="truncate font-mono text-slate-500 text-xs">{e.namespace}</span>
                </div>
                {e.message && <p className="mt-2 text-amber-300/80 text-xs">{e.message}</p>}
              </Link>
            </li>
          ))}
        </ul>
      )}

      <div className="border-white/10 border-t pt-6">
        {environments.length > 0 ? (
          <p className="text-slate-500 text-sm">Delete all environments before deleting the project.</p>
        ) : (
          <ConfirmDelete
            name={project}
            what="project"
            pending={remove.isPending}
            error={remove.error}
            onConfirm={() => remove.mutate()}
          />
        )}
      </div>
    </section>
