/**
 * Settings → Registries: registry logins every environment of the
 * organization pulls with (an environment's own login for the same registry
 * wins). A preset fills the server and says what the username and password
 * are; a login is tested before it is saved, and can be tested again,
 * rotated or deleted.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLinkIcon, KeyRoundIcon, PlusIcon, RefreshCwIcon } from 'lucide-react'
import { type FormEvent, useId, useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import {
  ConfirmAction,
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
  lastCheckTone,
  NAME_PATTERN,
  type OrgRegistry,
  presetNeedsServer,
  type RegistryCheck,
  type RegistryForm,
  type RegistryPreset,
  registryBody,
} from '@/lib/integrations'
import {
  createOrgRegistry,
  deleteOrgRegistry,
  orgRegistriesQuery,
  registryPresetsQuery,
  testNewOrgRegistry,
  testOrgRegistry,
  updateOrgRegistry,
} from '@/lib/integrations-api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'

const invalidate = (queryClient: ReturnType<typeof useQueryClient>) =>
  queryClient.invalidateQueries({ queryKey: ['org-registries'] })

/** A check's outcome, as a status line. */
function CheckLine({ check }: { check: RegistryCheck }) {
  const { t } = usePrefs()
  return (
    <div role="status" className="flex flex-wrap items-center gap-2 rounded-lg border p-3 text-sm">
      <ToneBadge tone={check.ok ? 'success' : 'danger'}>
        {check.ok ? t('registries.loginOk') : t('registries.loginFailed')}
      </ToneBadge>
      {check.error && (
        <span dir="auto" className="text-destructive">
          {check.error}
        </span>
      )}
    </div>
  )
}

function PresetCards({
  presets,
  value,
  onChange,
}: {
  presets: readonly RegistryPreset[]
  value: string
  onChange: (preset: RegistryPreset) => void
}) {
  const { t } = usePrefs()
  const name = useId()
  return (
    <fieldset className="grid gap-2">
      <legend className="mb-2 font-medium text-sm">{t('registries.preset')}</legend>
      <div className="grid grid-cols-2 gap-2 sm:grid-cols-3">
        {presets.map((p) => (
          <label
            key={p.id}
            className="flex cursor-pointer flex-col gap-1 rounded-lg border p-3 text-sm transition hover:bg-accent/40 has-[:checked]:border-primary has-[:checked]:bg-accent/60 has-[:focus-visible]:ring-[3px] has-[:focus-visible]:ring-ring/50"
          >
            <input
              type="radio"
              name={name}
              value={p.id}
              checked={value === p.id}
              onChange={() => onChange(p)}
              className="sr-only"
            />
            <span className="font-medium">{p.label}</span>
            <span dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
              {p.server ?? t('registries.yourServer')}
            </span>
          </label>
        ))}
      </div>
    </fieldset>
  )
}

const formFor = (preset: RegistryPreset): RegistryForm => ({
  preset: preset.id,
  name: preset.id,
  server: preset.server ?? '',
  username: '',
  password: '',
})

function AddRegistryForm({ presets, onDone }: { presets: readonly RegistryPreset[]; onDone: () => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const first = presets[0]
  const [form, setForm] = useState<RegistryForm | null>(() => (first ? formFor(first) : null))
  const [checked, setChecked] = useState<{ key: string; check: RegistryCheck } | null>(null)
  const preset = presets.find((p) => p.id === form?.preset)
  const body = form ? registryBody(form, preset) : null
  const key = JSON.stringify(body)
  const check = checked?.key === key ? checked.check : null

  const test = useMutation({
    mutationFn: () => (body ? testNewOrgRegistry(body) : Promise.reject(new Error('no preset'))),
    onSuccess: (result) => setChecked({ key, check: result }),
  })
  const create = useMutation({
    mutationFn: () => (body ? createOrgRegistry(body) : Promise.reject(new Error('no preset'))),
    onSuccess: async () => {
      await invalidate(queryClient)
      onDone()
    },
  })
  if (!form) return <Notice>{t('registries.noPresets')}</Notice>
  const set = (patch: Partial<RegistryForm>) => setForm({ ...form, ...patch })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (check?.ok) create.mutate()
    else test.mutate()
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <PresetCards presets={presets} value={form.preset} onChange={(p) => setForm(formFor(p))} />
      <div className="grid gap-4 sm:grid-cols-2">
        <TextInput
          label={t('registries.server')}
          name="server"
          dir="ltr"
          required={presetNeedsServer(preset)}
          value={form.server}
          placeholder="registry.example.com"
          hint={t('registries.serverHint')}
          onChange={(e) => set({ server: e.target.value })}
        />
        <TextInput
          label={t('registries.name')}
          name="name"
          dir="ltr"
          required
          maxLength={63}
          pattern={NAME_PATTERN}
          value={form.name}
          hint={t('git.nameHint')}
          onChange={(e) => set({ name: e.target.value })}
        />
        <TextInput
          label={t('registries.username')}
          name="username"
          dir="ltr"
          required
          autoComplete="off"
          value={form.username}
          hint={preset?.usernameHint}
          onChange={(e) => set({ username: e.target.value })}
        />
        <TextInput
          label={t('registries.password')}
          name="password"
          type="password"
          dir="ltr"
          required
          autoComplete="off"
          value={form.password}
          hint={preset?.passwordHint}
          onChange={(e) => set({ password: e.target.value })}
        />
      </div>
      {preset?.docsUrl && (
        <a
          href={preset.docsUrl}
          target="_blank"
          rel="noreferrer"
          className="inline-flex items-center gap-1 text-link text-sm hover:underline"
        >
          {fill(t('registries.docs'), { preset: preset.label })}
          <ExternalLinkIcon aria-hidden="true" className="size-3.5" />
        </a>
      )}
      {check && <CheckLine check={check} />}
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
          disabled={test.isPending || !form.username || !form.password}
          onClick={() => test.mutate()}
        >
          {test.isPending ? t('git.testing') : t('registries.test')}
        </Button>
        <Button type="submit" disabled={!check?.ok || create.isPending}>
          {create.isPending ? t('ui.saving') : t('registries.save')}
        </Button>
      </DialogFooter>
    </form>
  )
}

/** A new password (and, when it changed, username): the login is rotated. */
function RotateRegistry({ registry }: { registry: OrgRegistry }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const rotate = useMutation({
    mutationFn: (body: { username?: string; password: string }) => updateOrgRegistry(registry.id, body),
    onSuccess: async () => {
      await invalidate(queryClient)
      setOpen(false)
    },
  })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const data = new FormData(event.currentTarget)
    const username = String(data.get('username') ?? '').trim()
    const password = String(data.get('password') ?? '')
    rotate.mutate(username && username !== registry.username ? { username, password } : { password })
  }
  return (
    <FormDialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) rotate.reset()
      }}
      title={fill(t('registries.rotateTitle'), { name: registry.name })}
      description={t('registries.rotateHint')}
      trigger={
        <Button variant="ghost" size="sm">
          <KeyRoundIcon aria-hidden="true" />
          {t('registries.rotate')}
        </Button>
      }
    >
      <form onSubmit={onSubmit} className="grid gap-4">
        <TextInput
          label={t('registries.username')}
          name="username"
          dir="ltr"
          autoComplete="off"
          defaultValue={registry.username}
        />
        <TextInput
          label={t('registries.newPassword')}
          name="password"
          type="password"
          dir="ltr"
          required
          autoComplete="off"
        />
        <ErrorAlert error={rotate.error} />
        <DialogFooter>
          <DialogClose asChild>
            <Button type="button" variant="outline">
              {t('ui.cancel')}
            </Button>
          </DialogClose>
          <Button type="submit" disabled={rotate.isPending}>
            {rotate.isPending ? t('ui.saving') : t('registries.rotate')}
          </Button>
        </DialogFooter>
      </form>
    </FormDialog>
  )
}

function DeleteRegistry({ registry }: { registry: OrgRegistry }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const remove = useMutation({
    mutationFn: () => deleteOrgRegistry(registry.id),
    onSuccess: () => invalidate(queryClient),
  })
  return (
    <ConfirmAction
      size="sm"
      variant="ghost"
      label={t('registries.delete')}
      title={fill(t('registries.deleteTitle'), { name: registry.name })}
      description={t('registries.deleteConfirm')}
      pending={remove.isPending}
      error={remove.error}
      onConfirm={() => remove.mutateAsync()}
    />
  )
}

function RegistriesCard() {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [adding, setAdding] = useState(false)
  const registries = useQuery(orgRegistriesQuery)
  const presets = useQuery(registryPresetsQuery)
  const [result, setResult] = useState<{ name: string; check: RegistryCheck } | null>(null)
  const test = useMutation({
    mutationFn: (r: OrgRegistry) => testOrgRegistry(r.id),
    onSuccess: async (check, r) => {
      setResult({ name: r.name, check })
      await invalidate(queryClient)
    },
  })
  const forbidden = registries.error instanceof ApiError && registries.error.status === 403
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const presetLabel = (id: string) => presets.data?.find((p) => p.id === id)?.label ?? id

  const columns: Column<OrgRegistry>[] = [
    {
      id: 'name',
      header: t('registries.name'),
      sortValue: (r) => r.name,
      filterValue: (r) => `${r.name} ${r.server} ${r.username} ${r.preset}`,
      cell: (r) => (
        <div className="space-y-0.5">
          <p dir="ltr" className="text-start font-medium font-mono text-sm">
            {r.name}
          </p>
          <Tag>{presetLabel(r.preset)}</Tag>
        </div>
      ),
    },
    {
      id: 'server',
      header: t('registries.server'),
      sortValue: (r) => r.server,
      cell: (r) => (
        <div dir="ltr" className="text-start font-mono text-xs">
          <p>{r.server}</p>
          <p className="text-muted-foreground">{r.username}</p>
        </div>
      ),
    },
    {
      id: 'check',
      header: t('git.lastCheck'),
      sortValue: (r) => r.lastCheckedAt ?? 0,
      filterValue: (r) => r.lastError,
      className: 'whitespace-normal text-xs',
      cell: (r) => (
        <div className="space-y-1">
          <ToneBadge tone={lastCheckTone(r)}>
            {r.lastError
              ? t('registries.loginFailed')
              : r.lastCheckedAt
                ? t('registries.loginOk')
                : t('git.neverChecked')}
          </ToneBadge>
          {r.lastCheckedAt != null && (
            <time className="block text-muted-foreground" dateTime={new Date(r.lastCheckedAt).toISOString()}>
              {date.format(r.lastCheckedAt)}
            </time>
          )}
          {r.lastError && (
            <p dir="auto" className="line-clamp-2 text-destructive">
              {r.lastError}
            </p>
          )}
        </div>
      ),
    },
    {
      id: 'actions',
      header: t('git.actions'),
      hideHeader: true,
      className: 'text-end',
      cell: (r) => (
        <div className="flex flex-wrap justify-end gap-1">
          <Button variant="ghost" size="sm" disabled={test.isPending} onClick={() => test.mutate(r)}>
            <RefreshCwIcon aria-hidden="true" />
            {t('registries.test')}
          </Button>
          <RotateRegistry registry={r} />
          <DeleteRegistry registry={r} />
        </div>
      ),
    },
  ]

  return (
    <Section
      title={t('registries.title')}
      description={t('registries.hint')}
      actions={
        !forbidden && (
          <FormDialog
            open={adding}
            onOpenChange={setAdding}
            title={t('registries.add')}
            description={t('registries.addHint')}
            className="sm:max-w-2xl"
            trigger={
              <Button size="sm">
                <PlusIcon aria-hidden="true" />
                {t('registries.add')}
              </Button>
            }
          >
            {!adding ? null : presets.isError ? (
              <ErrorAlert error={presets.error} />
            ) : presets.isPending ? (
              <Loading lines={3} />
            ) : (
              <AddRegistryForm presets={presets.data} onDone={() => setAdding(false)} />
            )}
          </FormDialog>
        )
      }
    >
      {result && (
        <div className="space-y-1">
          <p className="font-medium text-sm">{fill(t('git.testedName'), { name: result.name })}</p>
          <CheckLine check={result.check} />
        </div>
      )}
      <ErrorAlert error={test.error} />
      {forbidden ? (
        <Notice>{t('registries.forbidden')}</Notice>
      ) : registries.isError ? (
        <ErrorAlert error={registries.error} />
      ) : (
        <DataTable
          label={t('registries.title')}
          columns={columns}
          rows={registries.data}
          rowKey={(r) => r.id}
          loading={registries.isPending}
          empty={t('registries.empty')}
          initialSort={{ id: 'name', desc: false }}
        />
      )}
    </Section>
  )
}

export function RegistriesPage() {
  const { t } = usePrefs()
  return (
    <div className="space-y-6">
      <PageHeader
        title={t('nav.registries')}
        description={t('registries.lead')}
        actions={
          <Button variant="outline" asChild>
            <Link to="/settings/integrations">{t('nav.integrations')}</Link>
          </Button>
        }
      />
      <RegistriesCard />
    </div>
  )
}
