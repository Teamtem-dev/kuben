import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { PlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  ConfirmDelete,
  Copyable,
  EmptyState,
  ErrorAlert,
  FormDialog,
  Loading,
  PageHeader,
  Section,
  SelectInput,
  Tag,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { Item, ItemActions, ItemContent, ItemDescription, ItemGroup, ItemTitle } from '@/components/ui/item'
import { when } from '@/lib/ops'
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
} from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'

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
    <li className="space-y-3 py-4 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="flex flex-wrap items-center gap-2">
          <span dir="ltr" className="font-medium font-mono">
            {claim.domain}
          </span>
          <ToneBadge tone={claim.status}>{tOr(`domains.status.${claim.status}`, claim.status)}</ToneBadge>
          {claim.method && <Tag>{claim.method}</Tag>}
        </p>
        <ConfirmDelete
          name={claim.domain}
          what={t('domains.claimWhat')}
          pending={revoke.isPending}
          error={revoke.error}
          onConfirm={() => revoke.mutate()}
        />
      </div>
      {claim.status === 'pending' && (
        <div className="space-y-3 text-sm">
          <p className="text-muted-foreground">{t('domains.txtHint')}</p>
          <div className="space-y-2">
            <Copyable value={claim.challengeName} />
            <Copyable value={claim.challengeValue} />
          </div>
          <div className="flex flex-wrap items-end gap-2">
            {providers.length > 0 && (
              <SelectInput
                label={t('domains.verifyWith')}
                value={provider}
                className="w-56"
                onChange={(e) => setProvider(e.target.value)}
              >
                <option value="">{t('domains.txtRecord')}</option>
                {providers.map((p) => (
                  <option key={p.id} value={p.name}>
                    {p.name}
                  </option>
                ))}
              </SelectInput>
            )}
            <Button disabled={verify.isPending} onClick={() => verify.mutate()}>
              {t('domains.verify')}
            </Button>
          </div>
          {claim.lastError && (
            <p role="status" dir="auto" className="text-warning text-xs">
              {claim.lastError}
              {claim.lastCheckedAt && ` · ${when(claim.lastCheckedAt, locale)}`}
            </p>
          )}
        </div>
      )}
      {claim.verifiedAt && (
        <p className="text-muted-foreground text-xs">
          {t('domains.verifiedAt')} {when(claim.verifiedAt, locale)}
        </p>
      )}
      <ErrorAlert error={verify.error} />
    </li>
  )
}

function ClaimForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const create = useMutation({
    mutationFn: (domain: string) => claimDomain(domain),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['domains'] })
      onDone()
    },
  })
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    create.mutate(String(new FormData(e.currentTarget).get('domain') ?? ''))
  }
  return (
    <form onSubmit={submit} className="grid gap-4">
      <TextInput label={t('domains.domain')} name="domain" required dir="ltr" placeholder="example.com" />
      <ErrorAlert error={create.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          {t('domains.claim')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function Claims({ providers }: { providers: DnsProvider[] }) {
  const { t } = usePrefs()
  const claims = useQuery(claimsQuery)
  const [claiming, setClaiming] = useState(false)
  return (
    <Section
      title={t('domains.claims')}
      actions={
        <FormDialog
          open={claiming}
          onOpenChange={setClaiming}
          title={t('domains.claim')}
          trigger={
            <Button size="sm">
              <PlusIcon aria-hidden="true" />
              {t('domains.claim')}
            </Button>
          }
        >
          <ClaimForm onDone={() => setClaiming(false)} />
        </FormDialog>
      }
    >
      <ErrorAlert error={claims.error} />
      {claims.isPending && <Loading />}
      {claims.data?.length === 0 && <EmptyState>{t('domains.empty')}</EmptyState>}
      {claims.data && claims.data.length > 0 && (
        <ul className="divide-y">
          {claims.data.map((c) => (
            <ClaimRow key={c.id} claim={c} providers={providers} />
          ))}
        </ul>
      )}
    </Section>
  )
}

function ProviderForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const add = useMutation({
    mutationFn: (form: FormData) =>
      addDnsProvider(String(form.get('name') ?? ''), 'cloudflare', String(form.get('token') ?? '')),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['dns-providers'] })
      onDone()
    },
  })
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    add.mutate(new FormData(e.currentTarget))
  }
  return (
    <form onSubmit={submit} className="grid gap-4">
      <TextInput label={t('domains.providerName')} name="name" required placeholder="cf" />
      <TextInput
        label={t('domains.cloudflareToken')}
        name="token"
        type="password"
        required
        autoComplete="off"
        dir="ltr"
        hint={t('domains.tokenHint')}
      />
      <ErrorAlert error={add.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={add.isPending}>
          {t('domains.addProvider')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function Providers({ providers }: { providers: DnsProvider[] }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const remove = useMutation({
    mutationFn: (id: string) => removeDnsProvider(id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['dns-providers'] }),
  })
  return (
    <Section
      title={t('domains.providers')}
      actions={
        <FormDialog
          open={adding}
          onOpenChange={setAdding}
          title={t('domains.addProvider')}
          trigger={
            <Button size="sm" variant="outline">
              <PlusIcon aria-hidden="true" />
              {t('domains.addProvider')}
            </Button>
          }
        >
          <ProviderForm onDone={() => setAdding(false)} />
        </FormDialog>
      }
    >
      <ErrorAlert error={remove.error} />
      {providers.length > 0 && (
        <ItemGroup className="gap-2">
          {providers.map((p) => (
            <Item key={p.id} role="listitem" variant="outline" size="sm">
              <ItemContent className="min-w-0">
                <ItemTitle>
                  <span dir="auto">{p.name}</span>
                  <Tag>{p.kind}</Tag>
                </ItemTitle>
                <ItemDescription>{when(p.createdAt, locale)}</ItemDescription>
              </ItemContent>
              <ItemActions>
                <Button
                  variant="ghost"
                  size="sm"
                  className="text-destructive hover:text-destructive"
                  disabled={remove.isPending}
                  onClick={() => remove.mutate(p.id)}
                >
                  {t('domains.removeProvider')}
                </Button>
              </ItemActions>
            </Item>
          ))}
        </ItemGroup>
      )}
    </Section>
  )
}

/** Domain claims and DNS provider accounts of the organization. */
export function DomainsPage() {
  const { t } = usePrefs()
  const providers = useQuery(dnsProvidersQuery)
  return (
    <div className="space-y-6">
      <PageHeader title={t('domains.title')} description={t('domains.lead')} />
      <ErrorAlert error={providers.error} />
      <Claims providers={providers.data ?? []} />
      <Providers providers={providers.data ?? []} />
    </div>
  )
}
