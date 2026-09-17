import { useQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { AuthLayout, Card, ErrorNote } from '../components/ui'
import { type PublicStatus, publicStatusQuery } from '../lib/api'
import { usePrefs } from '../lib/prefs'

const route = getRouteApi('/status/$slug')

/** The look of each public state. */
export const statusTones: Record<string, string> = {
  operational: 'border-ok/30 bg-ok/10 text-ok',
  degraded: 'border-warn/30 bg-warn/10 text-warn',
  majorOutage: 'border-danger/30 bg-danger/10 text-danger',
}

function Pill({ status }: { status: string }) {
  const { tOr } = usePrefs()
  return (
    <span
      className={`inline-flex shrink-0 items-center rounded-full border px-2 py-0.5 font-medium text-xs ${statusTones[status] ?? statusTones.degraded}`}
    >
      {tOr(`status.state.${status}`, status)}
    </span>
  )
}

function when(value: string, locale: string): string {
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString(locale)
}

function Page({ page }: { page: PublicStatus }) {
  const { t, locale } = usePrefs()
  return (
    <div className="w-full max-w-2xl space-y-6">
      <header className="space-y-3">
        <h1 className="font-semibold text-2xl">{page.title}</h1>
        <div
          role="status"
          className={`rounded-xl border px-4 py-3 font-medium ${statusTones[page.status] ?? statusTones.degraded}`}
        >
          {t(`status.summary.${page.status}` as 'status.summary.operational')}
        </div>
      </header>
      <Card title={t('status.components')}>
        {page.components.length === 0 ? (
          <p className="text-muted text-sm">{t('status.noComponents')}</p>
        ) : (
          <ul className="divide-y divide-line-soft">
            {page.components.map((c) => (
              <li key={c.name} className="flex items-center justify-between gap-3 py-2.5">
                <span dir="auto" className="text-sm">
                  {c.name}
                </span>
                <Pill status={c.status} />
              </li>
            ))}
          </ul>
        )}
      </Card>
      <Card title={t('status.incidents')}>
        {page.incidents.length === 0 ? (
          <p className="text-muted text-sm">{t('status.noIncidents')}</p>
        ) : (
          <ul className="divide-y divide-line-soft">
            {page.incidents.map((i) => (
              <li key={`${i.component}-${i.startedAt}`} className="space-y-0.5 py-2.5 text-sm">
                <p className="flex flex-wrap items-center gap-2">
                  <span dir="auto" className="font-medium">
                    {i.component}
                  </span>
                  <span className="text-muted">
                    {i.resolvedAt ? t('status.resolved') : t('status.ongoing')} ·{' '}
                    {t(`status.severity.${i.severity}` as 'status.severity.critical')}
                  </span>
                </p>
                <p className="text-subtle text-xs">
                  {when(i.startedAt, locale)}
                  {i.resolvedAt && ` → ${when(i.resolvedAt, locale)}`}
                </p>
              </li>
            ))}
          </ul>
        )}
      </Card>
      <p className="text-subtle text-xs">
        {t('status.updated')} {when(page.updatedAt, locale)}
      </p>
    </div>
  )
}

/** A project's public status page: no sign-in, nothing internal. */
export function StatusPage() {
  const { slug } = route.useParams()
  const { t } = usePrefs()
  const page = useQuery({ ...publicStatusQuery(slug), refetchInterval: 30_000, retry: false })
  return (
    <AuthLayout>
      {page.isPending && <p className="text-subtle text-sm">{t('common.loading')}</p>}
      {page.error && <ErrorNote error={page.error} />}
      {page.data && <Page page={page.data} />}
    </AuthLayout>
  )
}
