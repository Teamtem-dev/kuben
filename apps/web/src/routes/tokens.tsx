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
        title="API tokens"
        subtitle="For CI/CD and scripts. A token never has more rights than you, and can be limited to one project or environment."
      />

      <Card title="Create a token">
        <form onSubmit={onSubmit} className="grid gap-3 sm:grid-cols-3">
          <TextField label="Name" name="name" required placeholder="github-actions" maxLength={64} />
          <Select label="Role" name="role" defaultValue="developer">
            <option value="viewer">viewer — read only</option>
            <option value="developer">developer — deploy</option>
            <option value="admin">admin</option>
          </Select>
          <TextField
            label="Expires in (days)"
            name="days"
            type="number"
            min={1}
            max={365}
            defaultValue={90}
          />
          <TextField label="Project (optional)" name="project" placeholder="shop" />
          <TextField label="Environment (optional)" name="environment" placeholder="staging" />
          <div className="flex items-end">
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? 'Creating…' : 'Create token'}
            </Button>
          </div>
        </form>
        <div className="mt-3 space-y-3">
          <ErrorNote error={create.error} />
          {created && (
            <div
              role="status"
              className="space-y-2 rounded-lg border border-emerald-400/30 bg-emerald-400/5 p-3 text-sm"
            >
              <p>Copy the token now — it is shown only once.</p>
              <code className="block select-all break-all rounded bg-black/40 px-2 py-1 font-mono">
                {created}
              </code>
              <p className="text-slate-400 text-xs">GitHub Actions (store it as the secret KUBEN_TOKEN):</p>
              <pre className="overflow-x-auto rounded bg-black/40 p-2 font-mono text-slate-300 text-xs">{`curl -fsS -X PATCH "$KUBEN_URL/api/v1/projects/shop/environments/staging/apps/api" \\
  -H "Authorization: Bearer $KUBEN_TOKEN" -H 'Content-Type: application/json' \\
  -d "{\\"image\\": \\"ghcr.io/acme/api:$GITHUB_SHA\\"}"`}</pre>
            </div>
          )}
        </div>
      </Card>

      <Card title="Your tokens">
        <ErrorNote error={revoke.error} />
        {tokens.length === 0 ? (
          <Empty>No tokens yet.</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm">
              <thead className="text-slate-500 text-xs">
