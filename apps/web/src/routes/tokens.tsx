import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { type FormEvent, useState } from 'react'
import { Badge, Button, Card, Empty, ErrorNote, PageHeader, Select, TextField } from '../components/ui'
import { createToken, revokeToken, tokensQuery } from '../lib/api'

const when = (ms?: number | null) => (ms ? new Date(ms).toLocaleString() : '—')

export function TokensPage() {
  const { data: tokens } = useSuspenseQuery(tokensQuery)
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['tokens'] })
  const [created, setCreated] = useState<string | null>(null)

  const create = useMutation({
    mutationFn: createToken,
    onSuccess: async (result) => {
      setCreated(result.token)
      await refresh()
    },
  })
  const revoke = useMutation({ mutationFn: revokeToken, onSettled: refresh })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const text = (key: string) => String(form.get(key) ?? '').trim()
    create.mutate(
      {
        name: text('name'),
        role: text('role') || 'developer',
        project: text('project') || null,
        environment: text('environment') || null,
        expires_in_days: Number(text('days') || '90'),
      },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <section className="space-y-6">
      <PageHeader
