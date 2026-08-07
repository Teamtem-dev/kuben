import { useMutation, useQuery, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  Card,
  ConfirmDelete,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextArea,
  TextField,
} from '../components/ui'
import {
  appQuery,
  checkDomains,
  deleteApp,
  environmentsQuery,
  logsQuery,
  type PromoteResult,
  promoteApp,
  releasesQuery,
  restartApp,
  rollbackApp,
  runApp,
  type UpdateApp,
  updateApp,
  type Volume,
} from '../lib/api'
import { formatEnvLines, parseEnvLines } from '../lib/env'

const route = getRouteApi('/_authed/projects/$project/$environment/$app')

export function AppPage() {
  const { project, environment, app } = route.useParams()
  const { data } = useSuspenseQuery(appQuery(project, environment, app))
  const a = data.app
  const web = a.processes[0]
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })

