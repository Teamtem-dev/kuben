import { useQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { AlertCircleIcon, CheckCircle2Icon, TriangleAlertIcon } from 'lucide-react'
import { AuthShell, ErrorAlert, Loading, Section, ToneBadge } from '@/components/kit'
import { Alert, AlertTitle } from '@/components/ui/alert'
import { type PublicStatus, publicStatusQuery } from '@/lib/api'
import type { Tone } from '@/lib/ops'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

const route = getRouteApi('/status/$slug')

/** The tone of each public state. */
const stateTone: Record<string, Tone> = { operational: 'success', degraded: 'warning', majorOutage: 'danger' }

const degraded = { icon: TriangleAlertIcon, className: 'border-warning/30 bg-warning/5 text-warning' }
const banner: Record<string, typeof degraded> = {
  operational: { icon: CheckCircle2Icon, className: 'border-success/30 bg-success/5 text-success' },
  degraded,
  majorOutage: {
    icon: AlertCircleIcon,
    className: 'border-destructive/30 bg-destructive/5 text-destructive',
  },
}

function State({ status }: { status: string }) {
  const { tOr } = usePrefs()
  return <ToneBadge tone={stateTone[status] ?? 'warning'}>{tOr(`status.state.${status}`, status)}</ToneBadge>
}

function when(value: string, locale: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(locale)
}

function Page({ page }: { page: PublicStatus }) {
  const { t, locale } = usePrefs()
  const { icon: Icon, className } = banner[page.status] ?? degraded
  return (
    <div className="w-full max-w-2xl space-y-6">
      <header className="space-y-4">
        <h1 dir="auto" className="font-semibold text-2xl tracking-tight">
          {page.title}
        </h1>
        <Alert role="status" className={cn(className)}>
          <Icon aria-hidden="true" />
          <AlertTitle>{t(`status.summary.${page.status}` as 'status.summary.operational')}</AlertTitle>
        </Alert>
      </header>
      <Section title={t('status.components')}>
        {page.components.length === 0 ? (
          <p className="text-muted-foreground text-sm">{t('status.noComponents')}</p>
        ) : (
          <ul className="divide-y">
            {page.components.map((c) => (
              <li
                key={c.name}
                className="flex items-center justify-between gap-3 py-2.5 first:pt-0 last:pb-0"
              >
                <span dir="auto" className="text-sm">
                  {c.name}
                </span>
                <State status={c.status} />
              </li>
            ))}
          </ul>
        )}
      </Section>
      <Section title={t('status.incidents')}>
        {page.incidents.length === 0 ? (
          <p className="text-muted-foreground text-sm">{t('status.noIncidents')}</p>
        ) : (
          <ul className="divide-y">
            {page.incidents.map((i) => (
              <li
                key={`${i.component}-${i.startedAt}`}
                className="space-y-0.5 py-2.5 text-sm first:pt-0 last:pb-0"
              >
                <p className="flex flex-wrap items-center gap-2">
                  <span dir="auto" className="font-medium">
                    {i.component}
                  </span>
                  <span className="text-muted-foreground">
                    {i.resolvedAt ? t('status.resolved') : t('status.ongoing')} ·{' '}
                    {t(`status.severity.${i.severity}` as 'status.severity.critical')}
                  </span>
                </p>
                <p className="text-muted-foreground text-xs">
                  {when(i.startedAt, locale)}
                  {i.resolvedAt && ` → ${when(i.resolvedAt, locale)}`}
                </p>
              </li>
            ))}
          </ul>
        )}
      </Section>
      <p className="text-muted-foreground text-xs">
        {t('status.updated')} {when(page.updatedAt, locale)}
      </p>
    </div>
  )
}

/** A project's public status page: no sign-in, nothing internal. */
export function StatusPage() {
  const { slug } = route.useParams()
  const page = useQuery({ ...publicStatusQuery(slug), refetchInterval: 30_000, retry: false })
  return (
    <AuthShell>
      {page.isPending && <Loading />}
      {page.error && <ErrorAlert error={page.error} className="max-w-2xl" />}
      {page.data && <Page page={page.data} />}
    </AuthShell>
  )
}
