import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { type FormEvent, useState } from 'react'
import { Badge, Button, Card, Empty, ErrorNote, PageHeader, Select, TextField } from '../components/ui'
import { createToken, revokeToken, tokensQuery } from '../lib/api'
import { usePrefs } from '../lib/prefs'

const when = (locale: string, ms?: number | null) => (ms ? new Date(ms).toLocaleString(locale) : '—')

export function TokensPage() {
  const { t, locale } = usePrefs()
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
      <PageHeader title={t('nav.tokens')} subtitle={t('tokens.lead')} />

      <Card title={t('tokens.create')}>
        <form onSubmit={onSubmit} className="grid gap-3 sm:grid-cols-3">
          <TextField
            label={t('projects.name')}
            name="name"
            required
            placeholder="github-actions"
            maxLength={64}
          />
          <Select label={t('team.role')} name="role" defaultValue="developer">
            <option value="viewer">
              {t('team.role.viewer')} — {t('tokens.readOnly')}
            </option>
            <option value="developer">
              {t('team.role.developer')} — {t('tokens.deploy')}
            </option>
            <option value="admin">{t('team.role.admin')}</option>
          </Select>
          <TextField
            label={t('tokens.expiresIn')}
            name="days"
            type="number"
            min={1}
            max={365}
            defaultValue={90}
          />
          <TextField label={t('tokens.project')} name="project" placeholder="shop" />
          <TextField label={t('tokens.environment')} name="environment" placeholder="staging" />
          <div className="flex items-end">
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? t('ui.creating') : t('tokens.createButton')}
            </Button>
          </div>
        </form>
        <div className="mt-3 space-y-3">
          <ErrorNote error={create.error} />
          {created && (
            <div role="status" className="space-y-2 rounded-lg border border-ok/30 bg-ok/5 p-3 text-sm">
              <p>{t('tokens.copyNow')}</p>
              <code
                dir="ltr"
                className="block select-all break-all rounded bg-inset px-2 py-1 text-start font-mono"
              >
                {created}
              </code>
              <p className="text-muted-foreground text-xs">{t('tokens.githubActions')}</p>
              <pre
                dir="ltr"
                className="overflow-x-auto rounded bg-inset p-2 font-mono text-fg-soft text-xs"
              >{`curl -fsS -X PATCH "$KUBEN_URL/api/v1/projects/shop/environments/staging/apps/api" \\
  -H "Authorization: Bearer $KUBEN_TOKEN" -H 'Content-Type: application/json' \\
  -d "{\\"image\\": \\"ghcr.io/acme/api:$GITHUB_SHA\\"}"`}</pre>
            </div>
          )}
        </div>
      </Card>

      <Card title={t('tokens.yours')}>
        <ErrorNote error={revoke.error} />
        {tokens.length === 0 ? (
          <Empty>{t('tokens.empty')}</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-start text-sm">
              <thead className="text-subtle text-xs">
                <tr>
                  <th className="pb-2 font-medium">{t('projects.name')}</th>
                  <th className="pb-2 font-medium">{t('tokens.scope')}</th>
                  <th className="pb-2 font-medium">{t('tokens.lastUsed')}</th>
                  <th className="pb-2 font-medium">{t('tokens.expires')}</th>
                  <th className="pb-2" />
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {tokens.map((token) => (
                  <tr key={token.id} className={token.revoked ? 'opacity-50' : ''}>
                    <td className="py-2 pe-4">
                      <span dir="auto" className="font-medium">
                        {token.name}
                      </span>{' '}
                      <span dir="ltr" className="font-mono text-subtle text-xs">
                        {token.prefix}…
                      </span>
                    </td>
                    <td className="py-2 pe-4">
                      <Badge>{token.role}</Badge>{' '}
                      {token.environment ?? token.project ?? t('tokens.organization')}
                    </td>
                    <td className="py-2 pe-4 text-muted-foreground">{when(locale, token.last_used_at)}</td>
                    <td className="py-2 pe-4 text-muted-foreground">{when(locale, token.expires_at)}</td>
                    <td className="py-2 text-end">
                      {token.revoked ? (
                        <span className="text-subtle text-xs">{t('tokens.revoked')}</span>
                      ) : (
                        <Button
                          variant="ghost"
                          disabled={revoke.isPending}
                          onClick={() => revoke.mutate(token.id)}
                        >
                          {t('tokens.revoke')}
                        </Button>
                      )}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </Card>
    </section>
  )
}
