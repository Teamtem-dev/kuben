import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Empty, ErrorNote, PageHeader, Status, TextField } from '../components/ui'
import { createProject, projectQuery, projectsQuery } from '../lib/api'

export function ProjectsPage() {
  const { data: projects } = useSuspenseQuery(projectsQuery)
  const [creating, setCreating] = useState(false)

  return (
