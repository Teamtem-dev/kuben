/**
 * Settings → Integrations: the Git providers Kuben builds from. Token
 * connections to GitHub, GitLab, Gitea and Forgejo (checked before they are
 * saved, with the webhook to add for push builds and a repository browser),
 * and the GitHub App installations linked to the organization.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { FolderGit2Icon, KeyRoundIcon, PencilIcon, PlusIcon, RefreshCwIcon } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import { BranchPicker, CheckResult, ProviderMark, RepositoryPicker } from '@/components/git'
import {
  ConfirmAction,
  Copyable,
  EmptyState,
  ErrorAlert,
  FormDialog,
  Loading,
  Notice,
  PageHeader,
  Section,
  Tag,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import {
  absoluteUrl,
  type ConnectionForm,
  cardNeedsUrl,
  cardOf,
  cardPlaceholder,
  cardProvider,
  cardUrl,
  connectionBody,
  connectionChange,
  type GitConnection,
  type GitConnectionCheck,
  type GitRepository,
  type Installation,
  lastCheckTone,
  NAME_PATTERN,
  PROVIDER_CARDS,
  type ProviderCard,
  suggestName,
} from '@/lib/integrations'
import {
  createGitConnection,
  deleteGitConnection,
  gitConnectionsQuery,
  gitInstallationsQuery,
  linkGitInstallation,
  rotateGitWebhookSecret,
  testGitConnection,
  testNewGitConnection,
  updateGitConnection,
} from '@/lib/integrations-api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'

const invalidate = (queryClient: ReturnType<typeof useQueryClient>) =>
  queryClient.invalidateQueries({ queryKey: ['git-connections'] })

/** The provider cards of the add dialog: radio buttons that look like cards. */
function ProviderCards({ value, onChange }: { value: ProviderCard; onChange: (card: ProviderCard) => void }) {
  const { t } = usePrefs()
  const name = useId()
  return (
    <fieldset className="grid gap-2">
      <legend className="mb-2 font-medium text-sm">{t('git.provider')}</legend>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-4">
        {PROVIDER_CARDS.map((card) => (
          <label
            key={card}
            className="flex cursor-pointer flex-col items-start gap-2 rounded-lg border p-3 text-sm transition hover:bg-accent/40 has-[:checked]:border-primary has-[:checked]:bg-accent/60 has-[:focus-visible]:ring-[3px] has-[:focus-visible]:ring-ring/50"
          >
            <input
              type="radio"
              name={name}
              value={card}
              checked={value === card}
              onChange={() => onChange(card)}
              className="sr-only"
            />
            <ProviderMark card={card} />
            <span className="font-medium">{t(`git.card.${card}`)}</span>
            <span className="text-muted-foreground text-xs">{t(`git.cardHint.${card}`)}</span>
          </label>
        ))}
      </div>
    </fieldset>
  )
}

/**
 * A webhook secret, shown this once: the address and the secret to copy,
 * and where they go in the provider's settings.
 */
function WebhookSecretOnce({
  card,
  webhookUrl,
  secret,
}: {
  card: ProviderCard
  webhookUrl?: string | null
  secret: string
}) {
  const { t } = usePrefs()
  return (
    <div className="grid gap-3">
      <Notice tone="warning" role="alert">
        {t('git.secretOnce')}
      </Notice>
      {webhookUrl && (
        <div className="grid gap-1.5">
          <p className="font-medium text-sm">{t('git.webhookUrl')}</p>
          <Copyable value={absoluteUrl(webhookUrl, window.location.origin)} />
        </div>
      )}
      <div className="grid gap-1.5">
        <p className="font-medium text-sm">{t('git.webhookSecret')}</p>
        <Copyable value={secret} />
      </div>
      <p className="text-muted-foreground text-sm">{t(`git.secretSteps.${card}`)}</p>
    </div>
  )
}

function DoneFooter() {
  const { t } = usePrefs()
  return (
    <DialogFooter>
      <DialogClose asChild>
        <Button type="button">{t('git.done')}</Button>
      </DialogClose>
    </DialogFooter>
  )
}

const emptyForm = (card: ProviderCard): ConnectionForm => ({
  card,
  name: suggestName(card, cardUrl(card)),
  url: cardUrl(card),
  token: '',
  defaultBranch: '',
})

/** Add a connection: a provider, its URL and a token, tested before it is saved. */
function AddConnectionForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [form, setForm] = useState<ConnectionForm>(() => emptyForm('gitlab'))
  // The check belongs to the values it was made with; any change asks for a new one.
  const [checked, setChecked] = useState<{ key: string; check: GitConnectionCheck } | null>(null)
  const [nameTouched, setNameTouched] = useState(false)
  const body = connectionBody(form)
  const key = JSON.stringify(body)
  const check = checked?.key === key ? checked.check : null

  const test = useMutation({
    mutationFn: () => testNewGitConnection(body),
    onSuccess: (result) => setChecked({ key, check: result }),
  })
  // The created connection, while its webhook secret is shown (the only time it is).
  const [created, setCreated] = useState<GitConnection | null>(null)
  const create = useMutation({
    mutationFn: () => createGitConnection(body),
    onSuccess: async (connection) => {
      await invalidate(queryClient)
      if (connection.webhookSecret) setCreated(connection)
      else onDone()
    },
  })

  const set = (patch: Partial<ConnectionForm>) =>
    setForm((f) => {
      const next = { ...f, ...patch }
      return nameTouched ? next : { ...next, name: suggestName(next.card, next.url) }
    })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (check?.ok) create.mutate()
    else test.mutate()
  }

  if (created?.webhookSecret) {
    return (
      <div className="grid gap-4">
        <WebhookSecretOnce card={form.card} webhookUrl={created.webhookUrl} secret={created.webhookSecret} />
        <DoneFooter />
      </div>
    )
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <ProviderCards value={form.card} onChange={(card) => set({ card, url: cardUrl(card) })} />
      <div className="grid gap-4 sm:grid-cols-2">
        <TextInput
          label={t('git.url')}
          name="url"
          type="url"
          dir="ltr"
          required={cardNeedsUrl(form.card)}
          value={form.url}
          placeholder={cardPlaceholder(form.card)}
          hint={t(`git.urlHint.${form.card}`)}
          onChange={(e) => set({ url: e.target.value })}
        />
        <TextInput
          label={t('git.name')}
          name="name"
          dir="ltr"
          required
          maxLength={63}
          pattern={NAME_PATTERN}
          value={form.name}
          hint={t('git.nameHint')}
          onChange={(e) => {
            setNameTouched(true)
            setForm((f) => ({ ...f, name: e.target.value }))
          }}
        />
        <TextInput
          label={t('git.token')}
          name="token"
          type="password"
          dir="ltr"
          required
          autoComplete="off"
          value={form.token}
          hint={t(`git.tokenHint.${cardProvider(form.card)}`)}
          onChange={(e) => set({ token: e.target.value })}
        />
        <TextInput
          label={t('git.defaultBranch')}
          name="defaultBranch"
          dir="ltr"
          placeholder="main"
          value={form.defaultBranch}
          hint={t('git.defaultBranchHint')}
          onChange={(e) => set({ defaultBranch: e.target.value })}
        />
      </div>
      {check && <CheckResult check={check} />}
      <ErrorAlert error={test.error ?? create.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button
          type="button"
          variant={check?.ok ? 'outline' : 'default'}
          disabled={test.isPending || !body.token}
          onClick={() => test.mutate()}
        >
          {test.isPending ? t('git.testing') : t('git.test')}
        </Button>
        <Button type="submit" disabled={!check?.ok || create.isPending}>
          {create.isPending ? t('ui.saving') : t('git.save')}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** Change a connection: its name, URL, default branch, or a new token (rotation). */
function EditConnection({ connection }: { connection: GitConnection }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const update = useMutation({
    mutationFn: (form: Pick<ConnectionForm, 'name' | 'url' | 'token' | 'defaultBranch'>) =>
      updateGitConnection(connection.id, connectionChange(connection, form)),
    onSuccess: async () => {
      await invalidate(queryClient)
      setOpen(false)
    },
  })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    const field = (key: string) => String(data.get(key) ?? '')
    update.mutate({
      name: field('name'),
      url: field('url'),
      token: field('token'),
      defaultBranch: field('defaultBranch'),
    })
  }
  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) update.reset()
      }}
      title={fill(t('git.editTitle'), { name: connection.name })}
      description={t('git.editHint')}
      trigger={
        <Button variant="ghost" size="sm">
          <PencilIcon aria-hidden="true" />
          {t('git.edit')}
        </Button>
      }
    >
      <form onSubmit={onSubmit} className="grid gap-4">
        <TextInput
          label={t('git.name')}
          name="name"
          dir="ltr"
          required
          pattern={NAME_PATTERN}
          defaultValue={connection.name}
        />
        <TextInput label={t('git.url')} name="url" type="url" dir="ltr" defaultValue={connection.baseUrl} />
        <TextInput
          label={t('git.newToken')}
          name="token"
          type="password"
          dir="ltr"
          autoComplete="off"
          hint={fill(t('git.newTokenHint'), { hint: connection.tokenHint })}
        />
        <TextInput
          label={t('git.defaultBranch')}
          name="defaultBranch"
          dir="ltr"
          placeholder="main"
          defaultValue={connection.defaultBranch ?? ''}
        />
        <ErrorAlert error={update.error} />
        <DialogFooter>
          <DialogClose asChild>
            <Button type="button" variant="outline">
              {t('ui.cancel')}
            </Button>
          </DialogClose>
          <Button type="submit" disabled={update.isPending}>
            {update.isPending ? t('ui.saving') : t('ui.save')}
          </Button>
        </DialogFooter>
      </form>
    </FormDialog>
  )
}

/** A new webhook secret, after a confirmation; shown once, then never again. */
function RotateWebhookSecret({ connection }: { connection: GitConnection }) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  const rotate = useMutation({ mutationFn: () => rotateGitWebhookSecret(connection.id) })
  const title = fill(t('git.rotateSecretTitle'), { name: connection.name })
  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) rotate.reset()
      }}
      title={title}
      trigger={
        <Button variant="ghost" size="sm">
          <KeyRoundIcon aria-hidden="true" />
          {t('git.rotateSecret')}
        </Button>
      }
    >
      {rotate.data ? (
        <div className="grid gap-4">
          <WebhookSecretOnce
            card={cardOf(connection)}
            webhookUrl={rotate.data.webhookUrl ?? connection.webhookUrl}
            secret={rotate.data.webhookSecret}
          />
          <DoneFooter />
        </div>
      ) : (
        <div className="grid gap-4">
          <p className="text-muted-foreground text-sm">{t('git.rotateSecretConfirm')}</p>
          <ErrorAlert error={rotate.error} />
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                {t('ui.cancel')}
              </Button>
            </DialogClose>
            <Button variant="destructive" disabled={rotate.isPending} onClick={() => rotate.mutate()}>
              {t('git.rotateSecret')}
            </Button>
          </DialogFooter>
        </div>
      )}
    </FormDialog>
  )
}

function DeleteConnection({ connection }: { connection: GitConnection }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteGitConnection(connection.id),
    onSuccess: () => invalidate(queryClient),
  })
  const inUse = remove.error instanceof ApiError && remove.error.status === 409
  return (
    <ConfirmAction
      size="sm"
      variant="ghost"
      label={t('git.delete')}
      title={fill(t('git.deleteTitle'), { name: connection.name })}
      description={t('git.deleteConfirm')}
      pending={remove.isPending}
      error={inUse ? new Error(t('git.inUse')) : remove.error}
      onConfirm={() => remove.mutateAsync()}
    />
  )
}

/** Browse what a connection can read: repositories, then a repository's branches. */
function BrowseRepositories({ connection }: { connection: GitConnection }) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  const [repository, setRepository] = useState<GitRepository | null>(null)
  const [branch, setBranch] = useState('')
  return (
    <FormDialog
      open={open}
      onOpenChange={setOpen}
      title={fill(t('git.browseTitle'), { name: connection.name })}
      description={t('git.browseHint')}
      className="sm:max-w-xl"
      trigger={
        <Button variant="ghost" size="sm">
          <FolderGit2Icon aria-hidden="true" />
          {t('git.repositories')}
        </Button>
      }
    >
      {open && (
        <div className="grid gap-4">
          <RepositoryPicker
            connection={connection.id}
            value={repository?.fullName ?? ''}
            onChange={(r) => {
              setRepository(r)
              setBranch('')
            }}
          />
          {repository && (
            <BranchPicker
              key={repository.fullName}
              connection={connection.id}
              repository={repository.fullName}
              value={branch}
              fallback={repository.defaultBranch ?? connection.defaultBranch}
              onChange={setBranch}
            />
          )}
          <DialogFooter>
            <DialogClose asChild>
              <Button type="button" variant="outline">
                {t('git.close')}
              </Button>
            </DialogClose>
          </DialogFooter>
        </div>
      )}
    </FormDialog>
  )
}

/** Where to add the connection's webhook, in the provider's own words. */
function WebhookHelp({ connection }: { connection: GitConnection }) {
  const { t } = usePrefs()
  if (!connection.webhookUrl) {
    return <p className="text-muted-foreground text-xs">{t('git.noWebhook')}</p>
  }
  const card = cardOf(connection)
  return (
    <div className="space-y-1.5">
      <Copyable value={absoluteUrl(connection.webhookUrl, window.location.origin)} />
      <p className="text-muted-foreground text-xs">{t(`git.webhookHelp.${card}`)}</p>
    </div>
  )
}

function ConnectionsCard() {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const connections = useQuery(gitConnectionsQuery)
  const [result, setResult] = useState<{ name: string; check: GitConnectionCheck } | null>(null)
  const test = useMutation({
    mutationFn: (c: GitConnection) => testGitConnection(c.id),
    onSuccess: async (check, c) => {
      setResult({ name: c.name, check })
      await invalidate(queryClient)
    },
  })
  const forbidden = connections.error instanceof ApiError && connections.error.status === 403
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })

  const columns: Column<GitConnection>[] = [
    {
      id: 'name',
      header: t('git.connection'),
      sortValue: (c) => c.name,
      filterValue: (c) => `${c.name} ${c.provider} ${c.baseUrl} ${c.username ?? ''}`,
      className: 'whitespace-normal',
      cell: (c) => (
        <div className="flex items-start gap-3">
          <ProviderMark card={cardOf(c)} />
          <div className="min-w-0 space-y-0.5">
            <p dir="ltr" className="text-start font-medium font-mono text-sm">
              {c.name}
            </p>
            <p className="text-muted-foreground text-xs">
              {t(`git.card.${cardOf(c)}`)} ·{' '}
              <span dir="ltr" className="break-all font-mono">
                {c.baseUrl}
              </span>
            </p>
            {c.username && (
              <p className="text-muted-foreground text-xs">
                {t('git.signedInAs')}{' '}
                <span dir="ltr" className="font-mono">
                  {c.username}
                </span>
              </p>
            )}
          </div>
        </div>
      ),
    },
    {
      id: 'check',
      header: t('git.lastCheck'),
      sortValue: (c) => c.lastCheckedAt ?? 0,
      filterValue: (c) => c.lastError,
      className: 'whitespace-normal text-xs',
      cell: (c) => (
        <div className="space-y-1">
          <ToneBadge tone={lastCheckTone(c)}>
            {c.lastError ? t('git.checkFailed') : c.lastCheckedAt ? t('git.checkOk') : t('git.neverChecked')}
          </ToneBadge>
          {c.lastCheckedAt != null && (
            <time className="block text-muted-foreground" dateTime={new Date(c.lastCheckedAt).toISOString()}>
              {date.format(c.lastCheckedAt)}
            </time>
          )}
          {c.lastError && (
            <p dir="auto" className="line-clamp-2 text-destructive">
              {c.lastError}
            </p>
          )}
          {c.hasToken && (
            <p className="text-muted-foreground">{fill(t('git.tokenEnds'), { hint: c.tokenHint })}</p>
          )}
        </div>
      ),
    },
    {
      id: 'webhook',
      header: t('git.webhook'),
      className: 'min-w-64 max-w-md whitespace-normal',
      cell: (c) => <WebhookHelp connection={c} />,
    },
    {
      id: 'actions',
      header: t('git.actions'),
      hideHeader: true,
      className: 'text-end',
      cell: (c) => (
        <div className="flex flex-wrap justify-end gap-1">
          <Button variant="ghost" size="sm" disabled={test.isPending} onClick={() => test.mutate(c)}>
            <RefreshCwIcon aria-hidden="true" />
            {t('git.test')}
          </Button>
          <BrowseRepositories connection={c} />
          <EditConnection connection={c} />
          <RotateWebhookSecret connection={c} />
          <DeleteConnection connection={c} />
        </div>
      ),
    },
  ]

  return (
    <Section
      title={t('git.connections')}
      description={t('git.connectionsHint')}
      actions={
        !forbidden && (
          <FormDialog
            open={adding}
            onOpenChange={setAdding}
            title={t('git.add')}
            description={t('git.addHint')}
            className="sm:max-w-2xl"
            trigger={
              <Button size="sm">
                <PlusIcon aria-hidden="true" />
                {t('git.add')}
              </Button>
            }
          >
            {adding && <AddConnectionForm onDone={() => setAdding(false)} />}
          </FormDialog>
        )
      }
    >
      {result && (
        <div className="space-y-1">
          <p className="font-medium text-sm">{fill(t('git.testedName'), { name: result.name })}</p>
          <CheckResult check={result.check} />
        </div>
      )}
      <ErrorAlert error={test.error} />
      {forbidden ? (
        <Notice>{t('git.forbidden')}</Notice>
      ) : connections.isError ? (
        <ErrorAlert error={connections.error} />
      ) : (
        <DataTable
          label={t('git.connections')}
          columns={columns}
          rows={connections.data}
          rowKey={(c) => c.id}
          loading={connections.isPending}
          empty={t('git.empty')}
          initialSort={{ id: 'name', desc: false }}
        />
      )}
    </Section>
  )
}

function LinkInstallationForm({ onDone }: { onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const link = useMutation({
    mutationFn: linkGitInstallation,
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['git-installations'] })
      onDone()
    },
  })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    link.mutate(Number(new FormData(event.currentTarget).get('installationId')))
  }
  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <TextInput
        label={t('git.installationId')}
        name="installationId"
        type="number"
        min={1}
        required
        dir="ltr"
        hint={t('git.installationIdHint')}
      />
      <ErrorAlert error={link.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={link.isPending}>
          {t('git.link')}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** The GitHub App: installations linked to the organization, and linking another. */
function GithubAppCard() {
  const { t } = usePrefs()
  const [linking, setLinking] = useState(false)
  const installations = useQuery(gitInstallationsQuery)
  const unavailable =
    installations.error instanceof ApiError &&
    (installations.error.status === 404 || installations.error.status === 503)
  const forbidden = installations.error instanceof ApiError && installations.error.status === 403
  return (
    <Section
      title={t('git.githubApp')}
      description={t('git.githubAppHint')}
      actions={
        !forbidden &&
        !unavailable && (
          <FormDialog
            open={linking}
            onOpenChange={setLinking}
            title={t('git.linkInstallation')}
            trigger={
              <Button size="sm" variant="outline">
                <PlusIcon aria-hidden="true" />
                {t('git.linkInstallation')}
              </Button>
            }
          >
            {linking && <LinkInstallationForm onDone={() => setLinking(false)} />}
          </FormDialog>
        )
      }
    >
      {forbidden ? (
        <Notice>{t('git.forbidden')}</Notice>
      ) : unavailable ? (
        <Notice>{t('git.githubAppOff')}</Notice>
      ) : installations.isError ? (
        <ErrorAlert error={installations.error} />
      ) : installations.isPending ? (
        <Loading lines={2} />
      ) : installations.data.length === 0 ? (
        <EmptyState>{t('git.noInstallations')}</EmptyState>
      ) : (
        <ul className="divide-y rounded-lg border">
          {installations.data.map((i: Installation) => (
            <li key={i.installationId} className="flex flex-wrap items-center gap-3 px-3 py-2.5 text-sm">
              <ProviderMark card="github" />
              <span dir="ltr" className="font-medium font-mono">
                {i.account}
              </span>
              <Tag>#{i.installationId}</Tag>
              <ToneBadge tone={i.suspended ? 'warning' : 'success'} className="ms-auto">
                {i.suspended ? t('git.suspended') : t('git.active')}
              </ToneBadge>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

export function IntegrationsPage() {
  const { t } = usePrefs()
  return (
    <div className="space-y-6">
      <PageHeader
        title={t('nav.integrations')}
        description={t('git.lead')}
        actions={
          <Button variant="outline" asChild>
            <Link to="/settings/registries">{t('nav.registries')}</Link>
          </Button>
        }
      />
      <ConnectionsCard />
      <GithubAppCard />
    </div>
  )
}
