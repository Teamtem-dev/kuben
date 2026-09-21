import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Pill, Sparkline } from '../../components/ops'
import { Badge, Button, Card, ErrorNote, Select, TextField } from '../../components/ui'
import { bytes, cpu, when } from '../../lib/ops'
import {
  deleteImagePolicy,
  detachApp,
  dnsProvidersQuery,
  imagePolicyQuery,
  metricsQuery,
  putImagePolicy,
  syncAppDns,
} from '../../lib/ops-api'
import { usePrefs } from '../../lib/prefs'

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
    <Card
      title={t('usage.title')}
      actions={
        <fieldset className="flex gap-1">
          <legend className="sr-only">{t('usage.window')}</legend>
          {(['1h', '7d'] as const).map((w) => (
            <Button
              key={w}
              variant={w === window ? 'primary' : 'ghost'}
              aria-pressed={w === window}
              onClick={() => setWindow(w)}
            >
              {t(`usage.window.${w}`)}
            </Button>
          ))}
        </fieldset>
      }
    >
      <ErrorNote error={metrics.error} />
      {metrics.data && !metrics.data.available && (
        <p role="status" dir="auto" className="text-muted-foreground text-sm">
          {t('usage.unavailable')} {metrics.data.reason}
        </p>
      )}
      {metrics.data?.available && last && (
        <div className="grid gap-4 sm:grid-cols-2">
          <div className="space-y-1">
            <p className="flex items-baseline justify-between gap-2 text-sm">
              <span className="font-medium">{t('usage.cpu')}</span>
              <span dir="ltr">{cpu(last.cpuMillis, locale, t('usage.cores'))}</span>
            </p>
            <Sparkline values={points.map((p) => p.cpuMillis)} label={t('usage.cpuChart')} />
          </div>
          <div className="space-y-1">
            <p className="flex items-baseline justify-between gap-2 text-sm">
              <span className="font-medium">{t('usage.memory')}</span>
              <span dir="ltr">{bytes(last.memoryBytes, locale)}</span>
            </p>
            <Sparkline values={points.map((p) => p.memoryBytes)} label={t('usage.memoryChart')} />
          </div>
          <p className="text-subtle text-xs sm:col-span-2">
            {t('usage.since')} {when(points[0]?.at, locale)} · {points.length} {t('usage.points')}
          </p>
        </div>
      )}
    </Card>
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
    <Card title={t('imagePolicy.title')}>
      <form onSubmit={submit} className="space-y-3">
        <div className="grid gap-3 sm:grid-cols-3">
          <TextField
            label={t('imagePolicy.pattern')}
            name="pattern"
            required
            dir="ltr"
            defaultValue={p?.pattern ?? 'semver:^1'}
            hint={t('imagePolicy.patternHint')}
          />
          <TextField
            label={t('imagePolicy.repository')}
            name="repository"
            dir="ltr"
            defaultValue={p?.repository ?? ''}
            placeholder={t('imagePolicy.repositoryHint')}
          />
          <TextField
            label={t('imagePolicy.interval')}
            name="interval"
            type="number"
            min={1}
            max={1440}
            defaultValue={Math.round((p?.intervalSecs ?? 300) / 60)}
          />
        </div>
        <label className="flex items-center gap-2 text-sm">
          <input type="checkbox" name="enabled" defaultChecked={p?.enabled ?? true} />
          {t('imagePolicy.enabled')}
        </label>
        <p className="text-subtle text-xs">{t('imagePolicy.approvalHint')}</p>
        {p && (
          <dl className="grid gap-1 text-sm sm:grid-cols-[auto_1fr]">
            <dt className="text-muted-foreground">{t('imagePolicy.lastTag')}</dt>
            <dd dir="ltr" className="text-start font-mono">
              {p.lastTag ?? '—'} {p.lastDigest && <Badge>{p.lastDigest.slice(7, 19)}</Badge>}
            </dd>
            <dt className="text-muted-foreground">{t('imagePolicy.nextCheck')}</dt>
            <dd>{when(p.nextCheckAt, locale)}</dd>
            {p.lastError && (
              <>
                <dt className="text-muted-foreground">{t('imagePolicy.lastError')}</dt>
                <dd dir="auto" className="text-danger">
                  {p.lastError} {p.failures > 0 && <Pill tone="warning">{p.failures}</Pill>}
                </dd>
              </>
            )}
          </dl>
        )}
        <ErrorNote error={save.error ?? remove.error} />
        <div className="flex gap-2">
          <Button type="submit" disabled={save.isPending}>
            {p ? t('ops.save') : t('imagePolicy.follow')}
          </Button>
          {p && (
            <Button variant="ghost" disabled={remove.isPending} onClick={() => remove.mutate()}>
              {t('imagePolicy.stop')}
            </Button>
          )}
        </div>
      </form>
    </Card>
  )
}

/** Write the app's DNS records through a provider account (M5.2). */
export function DnsCard({ project, environment, app, domains }: AppRef & { domains: readonly string[] }) {
  const { t, tOr } = usePrefs()
  const providers = useQuery(dnsProvidersQuery)
  const [picked, setPicked] = useState('')
  const provider = picked || (providers.data?.[0]?.name ?? '')
  const sync = useMutation({ mutationFn: () => syncAppDns(project, environment, app, provider) })
  if (domains.length === 0 || !providers.data || providers.data.length === 0) return null
  return (
    <Card title={t('dns.title')}>
      <div className="flex flex-wrap items-end gap-2">
        <div className="w-56">
          <Select label={t('dns.provider')} value={provider} onChange={(e) => setPicked(e.target.value)}>
            {providers.data.map((p) => (
              <option key={p.id} value={p.name}>
                {p.name}
              </option>
            ))}
          </Select>
        </div>
        <Button disabled={sync.isPending || !provider} onClick={() => sync.mutate()}>
          {t('dns.sync')}
        </Button>
      </div>
      <p className="mt-2 text-subtle text-xs">{t('dns.hint')}</p>
      <ErrorNote error={sync.error} />
      {sync.data && (
        <ul className="mt-3 divide-y divide-line-soft text-sm">
          {sync.data.map((c) => (
            <li key={`${c.host}-${c.recordType}-${c.content}-${c.action}`} className="space-y-0.5 py-2">
              <p className="flex flex-wrap items-center gap-2">
                <Pill
                  tone={
                    c.action === 'conflict' || c.action === 'failed'
                      ? 'failed'
                      : c.action === 'skipped'
                        ? 'pending'
                        : 'active'
                  }
                >
                  {tOr(`dns.action.${c.action}`, c.action)}
                </Pill>
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
    </Card>
  )
}

/** Hand the app over: Kuben lets go and it keeps running (M4.11). */
export function DetachCard({ project, environment, app }: AppRef) {
  const { t } = usePrefs()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState('')
  const [reason, setReason] = useState('')
  const detach = useMutation({
    mutationFn: () => detachApp(project, environment, app, reason),
    onSuccess: () => navigate({ to: '/projects/$project/$environment', params: { project, environment } }),
  })
  if (!open) {
    return (
      <div>
        <Button variant="ghost" onClick={() => setOpen(true)}>
          {t('detach.open')}
        </Button>
      </div>
    )
  }
  return (
    <Card title={t('detach.title')}>
      <div className="space-y-3">
        <p className="text-fg-soft text-sm">{t('detach.lead')}</p>
        <TextField label={t('detach.reason')} value={reason} onChange={(e) => setReason(e.target.value)} />
        <TextField
          label={`${t('detach.confirm')} ${app}`}
          value={typed}
          onChange={(e) => setTyped(e.target.value)}
          autoComplete="off"
        />
        <ErrorNote error={detach.error} />
        <div className="flex gap-2">
          <Button
            variant="danger"
            disabled={typed !== app || reason.trim() === '' || detach.isPending}
            onClick={() => detach.mutate()}
          >
            {t('detach.submit')}
          </Button>
          <Button variant="secondary" onClick={() => setOpen(false)}>
            {t('ops.cancel')}
          </Button>
        </div>
      </div>
    </Card>
  )
}
