import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  CheckboxField,
  ErrorAlert,
  Loading,
  Section,
  SelectInput,
  Sparkline,
  Tag,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { bytes, cpu, when } from '@/lib/ops'
import {
  deleteImagePolicy,
  detachApp,
  dnsProvidersQuery,
  imagePolicyQuery,
  metricsQuery,
  putImagePolicy,
  syncAppDns,
} from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'

interface AppRef {
  project: string
  environment: string
  app: string
}

/** CPU and memory of the app (M5.5); missing data is said, never drawn as zero. */
export function UsageCard({ project, environment, app }: AppRef) {
  const { t, locale } = usePrefs()
  const [window, setWindow] = useState<'1h' | '7d'>('1h')
  const metrics = useQuery(metricsQuery(project, environment, app, window))
  const points = metrics.data?.points ?? []
  const last = points.at(-1)
  return (
    <Section
      title={t('usage.title')}
      actions={
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={window}
          onValueChange={(value) => value && setWindow(value as '1h' | '7d')}
          aria-label={t('usage.window')}
        >
          {(['1h', '7d'] as const).map((w) => (
            <ToggleGroupItem key={w} value={w} className="px-3">
              {t(`usage.window.${w}`)}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
      }
    >
      <ErrorAlert error={metrics.error} />
      {metrics.isPending && <Loading lines={2} />}
      {metrics.data && !metrics.data.available && (
        <p role="status" dir="auto" className="text-muted-foreground text-sm">
          {t('usage.unavailable')} {metrics.data.reason}
        </p>
      )}
      {metrics.data?.available && last && (
        <div className="grid gap-6 sm:grid-cols-2">
          <div className="space-y-2">
            <p className="flex items-baseline justify-between gap-2 text-sm">
              <span className="text-muted-foreground">{t('usage.cpu')}</span>
              <span dir="ltr" className="font-semibold text-lg tabular-nums">
                {cpu(last.cpuMillis, locale, t('usage.cores'))}
              </span>
            </p>
            <Sparkline values={points.map((p) => p.cpuMillis)} label={t('usage.cpuChart')} />
          </div>
          <div className="space-y-2">
            <p className="flex items-baseline justify-between gap-2 text-sm">
              <span className="text-muted-foreground">{t('usage.memory')}</span>
              <span dir="ltr" className="font-semibold text-lg tabular-nums">
                {bytes(last.memoryBytes, locale)}
              </span>
            </p>
            <Sparkline values={points.map((p) => p.memoryBytes)} label={t('usage.memoryChart')} />
          </div>
          <p className="text-muted-foreground text-xs sm:col-span-2">
            {t('usage.since')} {when(points[0]?.at, locale)} · {points.length} {t('usage.points')}
          </p>
        </div>
      )}
    </Section>
  )
}

/** The app follows a tag pattern of its repository (M5.4). */
export function ImagePolicyCard({ project, environment, app }: AppRef) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const policy = useQuery(imagePolicyQuery(project, environment, app))
  const refresh = () =>
    queryClient.invalidateQueries({ queryKey: ['image-policy', project, environment, app] })
  const save = useMutation({
    mutationFn: (form: FormData) =>
      putImagePolicy(project, environment, app, {
        pattern: String(form.get('pattern') ?? ''),
        repository: String(form.get('repository') ?? '') || null,
        intervalSecs: Number(form.get('interval')) * 60,
        enabled: form.get('enabled') === 'on',
      }),
    onSuccess: refresh,
  })
  const remove = useMutation({
    mutationFn: () => deleteImagePolicy(project, environment, app),
    onSuccess: refresh,
  })
  if (policy.isPending) return null
  const p = policy.data
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    save.mutate(new FormData(e.currentTarget))
  }
  return (
    <Section title={t('imagePolicy.title')} description={t('imagePolicy.approvalHint')}>
      <form onSubmit={submit} className="grid gap-4">
        <div className="grid gap-4 sm:grid-cols-3">
          <TextInput
            label={t('imagePolicy.pattern')}
            name="pattern"
            required
            dir="ltr"
            defaultValue={p?.pattern ?? 'semver:^1'}
            hint={t('imagePolicy.patternHint')}
          />
          <TextInput
            label={t('imagePolicy.repository')}
            name="repository"
            dir="ltr"
            defaultValue={p?.repository ?? ''}
            placeholder={t('imagePolicy.repositoryHint')}
          />
          <TextInput
            label={t('imagePolicy.interval')}
            name="interval"
            type="number"
            min={1}
            max={1440}
            defaultValue={Math.round((p?.intervalSecs ?? 300) / 60)}
          />
        </div>
        <CheckboxField label={t('imagePolicy.enabled')} name="enabled" defaultChecked={p?.enabled ?? true} />
        {p && (
          <dl className="grid gap-x-4 gap-y-1 rounded-md bg-muted/50 p-3 text-sm sm:grid-cols-[auto_1fr]">
            <dt className="text-muted-foreground">{t('imagePolicy.lastTag')}</dt>
            <dd dir="ltr" className="flex flex-wrap items-center gap-2 text-start font-mono">
              {p.lastTag ?? '—'} {p.lastDigest && <Tag>{p.lastDigest.slice(7, 19)}</Tag>}
            </dd>
            <dt className="text-muted-foreground">{t('imagePolicy.nextCheck')}</dt>
            <dd>{when(p.nextCheckAt, locale)}</dd>
            {p.lastError && (
              <>
                <dt className="text-muted-foreground">{t('imagePolicy.lastError')}</dt>
                <dd dir="auto" className="flex flex-wrap items-center gap-2 text-destructive">
                  {p.lastError} {p.failures > 0 && <ToneBadge tone="warning">{p.failures}</ToneBadge>}
                </dd>
              </>
            )}
          </dl>
        )}
        <ErrorAlert error={save.error ?? remove.error} />
        <div className="flex gap-2">
          <Button type="submit" variant={p ? 'secondary' : 'default'} disabled={save.isPending}>
            {p ? t('ops.save') : t('imagePolicy.follow')}
          </Button>
          {p && (
            <Button type="button" variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {t('imagePolicy.stop')}
            </Button>
          )}
        </div>
      </form>
    </Section>
  )
}

const dnsTone = (action: string) =>
  action === 'conflict' || action === 'failed' ? 'danger' : action === 'skipped' ? 'warning' : 'success'

/** Write the app's DNS records through a provider account (M5.2). */
export function DnsCard({ project, environment, app, domains }: AppRef & { domains: readonly string[] }) {
  const { t, tOr } = usePrefs()
  const providers = useQuery(dnsProvidersQuery)
  const [picked, setPicked] = useState('')
  const provider = picked || (providers.data?.[0]?.name ?? '')
  const sync = useMutation({ mutationFn: () => syncAppDns(project, environment, app, provider) })
  if (domains.length === 0 || !providers.data || providers.data.length === 0) return null
  return (
    <Section title={t('dns.title')} description={t('dns.hint')}>
      <div className="flex flex-wrap items-end gap-2">
        <SelectInput
          label={t('dns.provider')}
          value={provider}
          className="w-56"
          onChange={(e) => setPicked(e.target.value)}
        >
          {providers.data.map((p) => (
            <option key={p.id} value={p.name}>
              {p.name}
            </option>
          ))}
        </SelectInput>
        <Button disabled={sync.isPending || !provider} onClick={() => sync.mutate()}>
          {t('dns.sync')}
        </Button>
      </div>
      <ErrorAlert error={sync.error} />
      {sync.data && (
        <ul className="divide-y text-sm">
          {sync.data.map((c) => (
            <li key={`${c.host}-${c.recordType}-${c.content}-${c.action}`} className="space-y-0.5 py-2">
              <p className="flex flex-wrap items-center gap-2">
                <ToneBadge tone={dnsTone(c.action)}>{tOr(`dns.action.${c.action}`, c.action)}</ToneBadge>
                <span dir="ltr" className="font-mono text-xs">
                  {c.recordType} {c.host} {c.content && `→ ${c.content}`}
                </span>
              </p>
              {c.detail && (
                <p dir="auto" className="text-muted-foreground text-xs">
                  {c.detail}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

/** Hand the app over: Kuben lets go and it keeps running (M4.11). */
export function DetachCard({ project, environment, app }: AppRef) {
  const { t } = usePrefs()
  const navigate = useNavigate()
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const detach = useMutation({
    mutationFn: () => detachApp(project, environment, app, reason),
    onSuccess: () => navigate({ to: '/projects/$project/$environment', params: { project, environment } }),
  })
  const ready = typed === app && reason.trim() !== ''
  return (
    <Section
      title={t('detach.title')}
      description={t('detach.lead')}
      actions={
        <AlertDialog
          onOpenChange={(open) => {
            if (!open) {
              setTyped('')
              setReason('')
            }
          }}
        >
          <AlertDialogTrigger asChild>
            <Button variant="outline">{t('detach.open')}</Button>
          </AlertDialogTrigger>
          <AlertDialogContent>
            <form
              className="grid gap-4"
              onSubmit={(event) => {
                event.preventDefault()
                if (ready) detach.mutate()
              }}
            >
              <AlertDialogHeader>
                <AlertDialogTitle>{t('detach.title')}</AlertDialogTitle>
                <AlertDialogDescription>{t('detach.lead')}</AlertDialogDescription>
              </AlertDialogHeader>
              <TextInput
                label={t('detach.reason')}
                value={reason}
                onChange={(e) => setReason(e.target.value)}
              />
              <TextInput
                label={`${t('detach.confirm')} ${app}`}
                value={typed}
                onChange={(e) => setTyped(e.target.value)}
                autoComplete="off"
                dir="auto"
              />
              <ErrorAlert error={detach.error} />
              <AlertDialogFooter>
                <AlertDialogCancel type="button">{t('ops.cancel')}</AlertDialogCancel>
                <Button type="submit" variant="destructive" disabled={!ready || detach.isPending}>
                  {t('detach.submit')}
                </Button>
              </AlertDialogFooter>
            </form>
          </AlertDialogContent>
        </AlertDialog>
      }
    />
  )
}
