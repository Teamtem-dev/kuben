import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { type FormEvent, useState } from 'react'
import {
  CopyButton,
  EmptyState,
  ErrorAlert,
  PageHeader,
  Section,
  SelectInput,
  Tag,
  TextInput,
} from '@/components/kit'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { createToken, revokeToken, tokensQuery } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

const when = (locale: string, ms?: number | null) => (ms ? new Date(ms).toLocaleString(locale) : '—')

const EXAMPLE = `curl -fsS -X PATCH "$KUBEN_URL/api/v1/projects/shop/environments/staging/apps/api" \\
  -H "Authorization: Bearer $KUBEN_TOKEN" -H 'Content-Type: application/json' \\
  -d "{\\"image\\": \\"ghcr.io/acme/api:$GITHUB_SHA\\"}"`

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
    <div className="space-y-6">
      <PageHeader title={t('nav.tokens')} description={t('tokens.lead')} />

      <Section title={t('tokens.create')}>
        <form onSubmit={onSubmit} className="grid gap-4 sm:grid-cols-3">
          <TextInput
            label={t('projects.name')}
            name="name"
            required
            placeholder="github-actions"
            maxLength={64}
          />
          <SelectInput label={t('team.role')} name="role" defaultValue="developer">
            <option value="viewer">
              {t('team.role.viewer')} — {t('tokens.readOnly')}
            </option>
            <option value="developer">
              {t('team.role.developer')} — {t('tokens.deploy')}
            </option>
            <option value="admin">{t('team.role.admin')}</option>
          </SelectInput>
          <TextInput
            label={t('tokens.expiresIn')}
            name="days"
            type="number"
            min={1}
            max={365}
            defaultValue={90}
          />
          <TextInput label={t('tokens.project')} name="project" placeholder="shop" dir="ltr" />
          <TextInput label={t('tokens.environment')} name="environment" placeholder="staging" dir="ltr" />
          <div className="flex items-end">
            <Button type="submit" disabled={create.isPending}>
              {create.isPending ? t('ui.creating') : t('tokens.createButton')}
            </Button>
          </div>
        </form>
        <ErrorAlert error={create.error} />
        {created && (
          <Alert role="status" className="border-success/30 bg-success/5">
            <AlertDescription className="block space-y-2 text-foreground">
              <p className="font-medium text-success">{t('tokens.copyNow')}</p>
              <div className="flex flex-wrap items-center gap-2">
                <code
                  dir="ltr"
                  className="min-w-0 flex-1 select-all break-all rounded-md bg-muted px-2 py-1 text-start font-mono text-sm"
                >
                  {created}
                </code>
                <CopyButton value={created} />
              </div>
              <p className="text-muted-foreground text-xs">{t('tokens.githubActions')}</p>
              <pre dir="ltr" className="overflow-x-auto rounded-md bg-muted p-2 text-start font-mono text-xs">
                {EXAMPLE}
              </pre>
            </AlertDescription>
          </Alert>
        )}
      </Section>

      <Section title={t('tokens.yours')}>
        <ErrorAlert error={revoke.error} />
        {tokens.length === 0 ? (
          <EmptyState>{t('tokens.empty')}</EmptyState>
        ) : (
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead>{t('projects.name')}</TableHead>
                <TableHead>{t('tokens.scope')}</TableHead>
                <TableHead>{t('tokens.lastUsed')}</TableHead>
                <TableHead>{t('tokens.expires')}</TableHead>
                <TableHead>
                  <span className="sr-only">{t('tokens.revoke')}</span>
                </TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {tokens.map((token) => (
                <TableRow key={token.id} className={cn(token.revoked && 'text-muted-foreground')}>
                  <TableCell>
                    <span dir="auto" className="font-medium">
                      {token.name}
                    </span>{' '}
                    <span dir="ltr" className="font-mono text-muted-foreground text-xs">
                      {token.prefix}…
                    </span>
                  </TableCell>
                  <TableCell>
                    <span className="flex items-center gap-2">
                      <Tag>{token.role}</Tag>
                      {token.environment ?? token.project ?? t('tokens.organization')}
                    </span>
                  </TableCell>
                  <TableCell className="text-muted-foreground">{when(locale, token.last_used_at)}</TableCell>
                  <TableCell className="text-muted-foreground">{when(locale, token.expires_at)}</TableCell>
                  <TableCell className="text-end">
                    {token.revoked ? (
                      <span className="text-xs">{t('tokens.revoked')}</span>
                    ) : (
                      <Button
                        variant="ghost"
                        size="sm"
                        className="text-destructive hover:text-destructive"
                        disabled={revoke.isPending}
                        onClick={() => revoke.mutate(token.id)}
                      >
                        {t('tokens.revoke')}
                      </Button>
                    )}
                  </TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
      </Section>
    </div>
  )
}
