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
