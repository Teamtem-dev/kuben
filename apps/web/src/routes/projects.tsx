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
