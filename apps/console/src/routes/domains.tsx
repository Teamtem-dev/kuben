import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { type FormEvent, useState } from 'react'
import { Copyable, Pill } from '../components/ops'
import {
  Badge,
  Button,
  Card,
  ConfirmDelete,
  Empty,
  ErrorNote,
  PageHeader,
  Select,
  TextField,
} from '../components/ui'
import { when } from '../lib/ops'
import {
  addDnsProvider,
  type Claim,
  claimDomain,
  claimsQuery,
  type DnsProvider,
  dnsProvidersQuery,
  removeDnsProvider,
  revokeClaim,
  verifyClaim,
} from '../lib/ops-api'
import { usePrefs } from '../lib/prefs'

function ClaimRow({ claim, providers }: { claim: Claim; providers: DnsProvider[] }) {
  const { t, tOr, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [provider, setProvider] = useState('')
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['domains'] })
  const verify = useMutation({
    mutationFn: () => verifyClaim(claim.id, provider || undefined),
    onSuccess: refresh,
  })
  const revoke = useMutation({ mutationFn: () => revokeClaim(claim.id), onSuccess: refresh })
  return (
    <li className="space-y-3 py-4">
      <p className="flex flex-wrap items-center gap-2">
        <span dir="ltr" className="font-medium font-mono">
          {claim.domain}
        </span>
        <Pill tone={claim.status}>{tOr(`domains.status.${claim.status}`, claim.status)}</Pill>
        {claim.method && <Badge>{claim.method}</Badge>}
      </p>
      {claim.status === 'pending' && (
        <div className="space-y-2 text-sm">
          <p className="text-muted">{t('domains.txtHint')}</p>
          <Copyable value={claim.challengeName} label={t('ops.copy')} />
          <Copyable value={claim.challengeValue} label={t('ops.copy')} />
          <div className="flex flex-wrap items-end gap-2">
            {providers.length > 0 && (
              <div className="w-56">
                <Select
                  label={t('domains.verifyWith')}
                  value={provider}
                  onChange={(e) => setProvider(e.target.value)}
                >
                  <option value="">{t('domains.txtRecord')}</option>
                  {providers.map((p) => (
                    <option key={p.id} value={p.name}>
                      {p.name}
                    </option>
                  ))}
                </Select>
              </div>
            )}
            <Button disabled={verify.isPending} onClick={() => verify.mutate()}>
              {t('domains.verify')}
            </Button>
          </div>
          {claim.lastError && (
            <p role="status" dir="auto" className="text-warn text-xs">
              {claim.lastError}
              {claim.lastCheckedAt && ` · ${when(claim.lastCheckedAt, locale)}`}
            </p>
          )}
        </div>
      )}
      {claim.verifiedAt && (
        <p className="text-subtle text-xs">
          {t('domains.verifiedAt')} {when(claim.verifiedAt, locale)}
        </p>
      )}
      <ErrorNote error={verify.error ?? revoke.error} />
      <ConfirmDelete
        name={claim.domain}
        what={t('domains.claimWhat')}
        pending={revoke.isPending}
        error={revoke.error}
        onConfirm={() => revoke.mutate()}
      />
    </li>
  )
}

function Claims({ providers }: { providers: DnsProvider[] }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const claims = useQuery(claimsQuery)
  const create = useMutation({
    mutationFn: (domain: string) => claimDomain(domain),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['domains'] }),
  })
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    create.mutate(String(new FormData(e.currentTarget).get('domain') ?? ''))
  }
  return (
    <Card title={t('domains.claims')}>
      <form onSubmit={submit} className="flex flex-wrap items-end gap-2">
        <div className="min-w-64 flex-1">
          <TextField label={t('domains.domain')} name="domain" required dir="ltr" placeholder="example.com" />
        </div>
        <Button type="submit" disabled={create.isPending}>
          {t('domains.claim')}
        </Button>
      </form>
      <div className="mt-3 space-y-3">
        <ErrorNote error={create.error ?? claims.error} />
        {claims.data?.length === 0 && <Empty>{t('domains.empty')}</Empty>}
        <ul className="divide-y divide-line-soft">
          {claims.data?.map((c) => (
            <ClaimRow key={c.id} claim={c} providers={providers} />
          ))}
        </ul>
      </div>
    </Card>
  )
}

function Providers({ providers }: { providers: DnsProvider[] }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['dns-providers'] })
  const add = useMutation({
    mutationFn: (form: FormData) =>
      addDnsProvider(String(form.get('name') ?? ''), 'cloudflare', String(form.get('token') ?? '')),
    onSuccess: refresh,
  })
  const remove = useMutation({ mutationFn: (id: string) => removeDnsProvider(id), onSuccess: refresh })
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    add.mutate(new FormData(e.currentTarget))
    e.currentTarget.reset()
  }
  return (
    <Card title={t('domains.providers')}>
      <form onSubmit={submit} className="grid gap-3 sm:grid-cols-[1fr_1fr_auto] sm:items-end">
        <TextField label={t('domains.providerName')} name="name" required placeholder="cf" />
        <TextField
          label={t('domains.cloudflareToken')}
          name="token"
          type="password"
          required
          autoComplete="off"
          hint={t('domains.tokenHint')}
        />
        <Button type="submit" disabled={add.isPending}>
          {t('domains.addProvider')}
        </Button>
      </form>
      <ErrorNote error={add.error ?? remove.error} />
      <ul className="mt-3 divide-y divide-line-soft">
        {providers.map((p) => (
          <li key={p.id} className="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
            <span className="flex items-center gap-2">
              <span dir="auto" className="font-medium">
                {p.name}
              </span>
              <Badge>{p.kind}</Badge>
              <span className="text-subtle text-xs">{when(p.createdAt, locale)}</span>
            </span>
            <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate(p.id)}>
              {t('domains.removeProvider')}
            </Button>
          </li>
        ))}
      </ul>
    </Card>
  )
}

/** Domain claims and DNS provider accounts of the organization. */
export function DomainsPage() {
  const { t } = usePrefs()
  const providers = useQuery(dnsProvidersQuery)
  return (
    <section className="space-y-6">
      <PageHeader title={t('domains.title')} subtitle={t('domains.lead')} />
      <ErrorNote error={providers.error} />
      <Claims providers={providers.data ?? []} />
      <Providers providers={providers.data ?? []} />
    </section>
  )
}
