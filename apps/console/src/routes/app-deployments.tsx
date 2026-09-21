import { useQuery } from '@tanstack/react-query'
import { Card, ErrorNote } from '../components/ui'
import { type Deployment, deploymentsQuery, isFinalPhase } from '../lib/api'
import { usePrefs } from '../lib/prefs'

type Where = { project: string; environment: string; app: string }

const tone = (phase: string) =>
  phase === 'succeeded' || phase === 'recovered'
    ? 'bg-ok'
    : phase === 'failed' || phase === 'recoveryFailed' || phase === 'manualActionRequired'
      ? 'bg-danger-solid'
      : isFinalPhase(phase)
        ? 'bg-subtle'
        : 'bg-brand'

/** `12.4s`, `3m 05s`. */
export function duration(ms: number): string {
  const seconds = Math.max(0, ms) / 1000
  if (seconds < 60) return `${seconds.toFixed(seconds < 10 ? 1 : 0)}s`
  const m = Math.floor(seconds / 60)
  return `${m}m ${String(Math.round(seconds % 60)).padStart(2, '0')}s`
}

function Timeline({ run }: { run: Deployment }) {
  const { tOr, locale } = usePrefs()
  const phase = (p: string) => tOr(`phase.${p}`, p)
  const time = new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  return (
    <ol className="mt-3 space-y-2 border-line border-s ps-4">
      {run.timeline.map((step, i) => {
        const next = run.timeline[i + 1]
        return (
          <li key={`${step.phase}-${step.at}`} className="relative text-sm">
            <span
              aria-hidden="true"
              className={`-start-[1.3rem] absolute top-1.5 size-2 rounded-full ${tone(step.phase)}`}
            />
            <span className="font-medium">{phase(step.phase)}</span>{' '}
            <time dateTime={new Date(step.at).toISOString()} className="text-subtle text-xs">
              {time.format(step.at)}
            </time>
            {next && <span className="text-subtle text-xs"> · {duration(next.at - step.at)}</span>}
          </li>
        )
      })}
    </ol>
  )
}

export function DeploymentsCard({ project, environment, app }: Where) {
  const { t, tOr, locale } = usePrefs()
  const runs = useQuery({ ...deploymentsQuery(project, environment, app), retry: false })
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  return (
    <Card title={t('deployments.title')}>
      {runs.isError ? (
        <ErrorNote error={runs.error} />
      ) : !runs.data?.length ? (
        <p className="text-subtle text-sm">{runs.isLoading ? t('common.loading') : t('deployments.empty')}</p>
      ) : (
        <ul className="divide-y divide-line-soft">
          {runs.data.map((run, i) => (
            <li key={run.run} className="py-3 first:pt-0 last:pb-0">
              <details open={i === 0}>
                <summary className="flex cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 text-sm">
                  <span className={`size-2 shrink-0 rounded-full ${tone(run.phase)}`} aria-hidden="true" />
                  <span className="font-medium">
                    {t('deployments.revision')} {run.generation}
                  </span>
                  <span className="text-muted-foreground">
                    {tOr(`deployments.reason.${run.reason}`, run.reason)}
                  </span>
                  <span>{tOr(`phase.${run.phase}`, run.phase)}</span>
                  <span className="text-subtle text-xs">
                    {date.format(run.created_at)} · {t('deployments.by')} {run.requested_by}
                  </span>
                </summary>
                {run.image && (
                  <p dir="ltr" className="mt-2 truncate text-start font-mono text-subtle text-xs">
                    {run.image}
                  </p>
                )}
                <Timeline run={run} />
              </details>
            </li>
          ))}
        </ul>
      )}
    </Card>
  )
}
