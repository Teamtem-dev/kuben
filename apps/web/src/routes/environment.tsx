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
