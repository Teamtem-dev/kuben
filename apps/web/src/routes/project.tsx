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
