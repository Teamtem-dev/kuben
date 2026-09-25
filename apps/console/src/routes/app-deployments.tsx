import { useQuery } from '@tanstack/react-query'
import { ChevronDownIcon, ChevronRightIcon } from 'lucide-react'
import { useState } from 'react'
import { ErrorAlert, Section } from '@/components/kit'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { Skeleton } from '@/components/ui/skeleton'
import { type Deployment, deploymentsQuery, isFinalPhase } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

type Where = { project: string; environment: string; app: string }

const tone = (phase: string) =>
  phase === 'succeeded' || phase === 'recovered'
    ? 'bg-success'
    : phase === 'failed' || phase === 'recoveryFailed' || phase === 'manualActionRequired'
      ? 'bg-destructive'
      : isFinalPhase(phase)
        ? 'bg-muted-foreground'
        : 'bg-primary'

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
    <ol className="ms-1 mt-3 space-y-2 border-s ps-4">
      {run.timeline.map((step, i) => {
        const next = run.timeline[i + 1]
        return (
          <li key={`${step.phase}-${step.at}`} className="relative text-sm">
            <span
              aria-hidden="true"
              className={cn('-start-[1.3rem] absolute top-1.5 size-2 rounded-full', tone(step.phase))}
            />
            <span className="font-medium">{phase(step.phase)}</span>{' '}
            <time dateTime={new Date(step.at).toISOString()} className="text-muted-foreground text-xs">
              {time.format(step.at)}
            </time>
            {next && <span className="text-muted-foreground text-xs"> · {duration(next.at - step.at)}</span>}
          </li>
        )
      })}
    </ol>
  )
}

/** One run: its summary, and when open its image and timeline. */
function Run({ run, defaultOpen }: { run: Deployment; defaultOpen: boolean }) {
  const { t, tOr, locale } = usePrefs()
  const [open, setOpen] = useState(defaultOpen)
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const Chevron = open ? ChevronDownIcon : ChevronRightIcon
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex w-full cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 rounded-md text-start text-sm outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50">
        <Chevron aria-hidden="true" className="size-4 shrink-0 text-muted-foreground rtl:-scale-x-100" />
        <span className={cn('size-2 shrink-0 rounded-full', tone(run.phase))} aria-hidden="true" />
        <span className="font-medium">
          {t('deployments.revision')} {run.generation}
        </span>
        <span className="text-muted-foreground">{tOr(`deployments.reason.${run.reason}`, run.reason)}</span>
        <span>{tOr(`phase.${run.phase}`, run.phase)}</span>
        <span className="text-muted-foreground text-xs">
          {date.format(run.created_at)} · {t('deployments.by')} <span dir="ltr">{run.requested_by}</span>
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent className="ps-6">
        {run.image && (
          <p dir="ltr" className="mt-2 truncate text-start font-mono text-muted-foreground text-xs">
            {run.image}
          </p>
        )}
        <Timeline run={run} />
      </CollapsibleContent>
    </Collapsible>
  )
}

/** The app's deployment runs, newest first, each with its phase timeline. */
export function DeploymentsCard({ project, environment, app }: Where) {
  const { t } = usePrefs()
  const runs = useQuery({ ...deploymentsQuery(project, environment, app), retry: false })
  return (
    <Section title={t('deployments.title')}>
      {runs.isError ? (
        <ErrorAlert error={runs.error} />
      ) : runs.isLoading ? (
        <div className="space-y-3" role="status" aria-label={t('common.loading')}>
          <Skeleton className="h-6 w-2/3" />
          <Skeleton className="h-6 w-1/2" />
        </div>
      ) : !runs.data?.length ? (
        <p className="text-muted-foreground text-sm">{t('deployments.empty')}</p>
      ) : (
        <ul className="divide-y">
          {runs.data.map((run, i) => (
            <li key={run.run} className="py-3 first:pt-0 last:pb-0">
              <Run run={run} defaultOpen={i === 0} />
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}
