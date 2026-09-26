import { keepPreviousData, useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ExternalLinkIcon } from 'lucide-react'
import { useState } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import { ErrorAlert, PageHeader, SwitchField, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { incidentState, when } from '@/lib/ops'
import { acknowledgeIncident, type Incident, incidentsQuery, resolveIncident } from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'

const SEVERITY_RANK: Record<string, number> = { critical: 0, warning: 1, info: 2 }
const STATE_RANK = { open: 0, acknowledged: 1, resolved: 2 } as const

/** Where the incident happened: a link to the app (or environment, project) when it names one. */
export function IncidentWhere({ incident }: { incident: Incident }) {
  const { project, environment, app } = incident
  if (!project) return null
  const label = [project, environment, app].filter(Boolean).join('/')
  const className = 'font-mono text-link text-xs hover:underline'
  if (environment && app) {
    return (
      <Link
        to="/projects/$project/$environment/$app"
        params={{ project, environment, app }}
        dir="ltr"
        className={className}
      >
        {label}
      </Link>
    )
  }
  if (environment) {
    return (
      <Link
        to="/projects/$project/$environment"
        params={{ project, environment }}
        dir="ltr"
        className={className}
      >
        {label}
      </Link>
    )
  }
  return (
    <Link to="/projects/$project" params={{ project }} dir="ltr" className={className}>
      {label}
    </Link>
  )
}

/** Acknowledge or resolve, and the error when that fails. */
function Actions({ incident }: { incident: Incident }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['incidents'] })
  const acknowledge = useMutation({ mutationFn: () => acknowledgeIncident(incident.id), onSuccess: refresh })
  const resolve = useMutation({ mutationFn: () => resolveIncident(incident.id), onSuccess: refresh })
  const state = incidentState(incident)
  if (state === 'resolved') return null
  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex gap-2">
        {state === 'open' && (
          <Button
            variant="outline"
            size="sm"
            disabled={acknowledge.isPending}
            onClick={() => acknowledge.mutate()}
          >
            {t('incidents.acknowledge')}
          </Button>
        )}
        <Button size="sm" disabled={resolve.isPending} onClick={() => resolve.mutate()}>
          {t('incidents.resolve')}
        </Button>
      </div>
      <ErrorAlert error={acknowledge.error ?? resolve.error} className="max-w-xs whitespace-normal" />
    </div>
  )
}

/** Incidents of the organization: open ones, or all. */
export function IncidentsPage() {
  const { t, tOr, locale } = usePrefs()
  const [all, setAll] = useState(false)
  // The previous list stays while the other one loads: the switch keeps its place and focus.
  const incidents = useQuery({ ...incidentsQuery(all), placeholderData: keepPreviousData })
  const severity = (i: Incident) => tOr(`incidents.severity.${i.severity}`, i.severity)

  const columns: Column<Incident>[] = [
    {
      id: 'severity',
      header: t('incidents.severity'),
      sortValue: (i) => SEVERITY_RANK[i.severity] ?? 9,
      filterValue: (i) => `${severity(i)} ${i.severity}`,
      cell: (i) => <ToneBadge tone={i.severity}>{severity(i)}</ToneBadge>,
    },
    {
      id: 'incident',
      header: t('incidents.incident'),
      sortValue: (i) => i.title,
      filterValue: (i) =>
        [i.title, i.detail, i.kind, i.project, i.environment, i.app].filter(Boolean).join(' '),
      className: 'min-w-64 whitespace-normal',
      cell: (i) => (
        <div className="space-y-1">
          <p dir="auto" className="font-medium">
            {i.title}
          </p>
          {i.detail && (
            <p dir="auto" className="text-muted-foreground text-xs">
              {i.detail}
            </p>
          )}
          <p className="flex flex-wrap items-center gap-x-3 gap-y-1">
            <span dir="ltr" className="font-mono text-muted-foreground text-xs">
              {i.kind}
            </span>
            <IncidentWhere incident={i} />
            {i.runbook && (
              <a
                href={i.runbook}
                target="_blank"
                rel="noopener noreferrer"
                className="inline-flex items-center gap-1 text-link text-xs hover:underline"
              >
                {t('incidents.runbook')}
                <ExternalLinkIcon aria-hidden="true" className="size-3" />
              </a>
            )}
          </p>
        </div>
      ),
    },
    {
      id: 'state',
      header: t('incidents.state'),
      sortValue: (i) => STATE_RANK[incidentState(i)],
      filterValue: (i) => t(`incidents.state.${incidentState(i)}`),
      className: 'whitespace-normal',
      cell: (i) => {
        const state = incidentState(i)
        const by = state === 'resolved' ? i.resolvedBy : state === 'acknowledged' ? i.acknowledgedBy : null
        const at = state === 'resolved' ? i.resolvedAt : state === 'acknowledged' ? i.acknowledgedAt : null
        return (
          <div className="space-y-1">
            <ToneBadge tone={state}>{t(`incidents.state.${state}`)}</ToneBadge>
            {at && (
              <p className="text-muted-foreground text-xs">
                {when(at, locale)}
                {by && (
                  <>
                    {' · '}
                    <span dir="ltr">{by}</span>
                  </>
                )}
              </p>
            )}
          </div>
        )
      },
    },
    {
      id: 'opened',
      header: t('incidents.opened'),
      sortValue: (i) => Date.parse(i.openedAt),
      className: 'text-muted-foreground text-xs',
      cell: (i) => <time dateTime={i.openedAt}>{when(i.openedAt, locale)}</time>,
    },
    {
      id: 'lastSeen',
      header: t('incidents.lastSeen'),
      sortValue: (i) => Date.parse(i.lastSeenAt),
      className: 'text-muted-foreground text-xs',
      cell: (i) => (
        <>
          <time dateTime={i.lastSeenAt}>{when(i.lastSeenAt, locale)}</time>
          <span className="block">
            {t('incidents.occurrences')} {i.occurrences}
          </span>
        </>
      ),
    },
    {
      id: 'actions',
      header: t('incidents.actions'),
      hideHeader: true,
      className: 'text-end',
      cell: (i) => <Actions incident={i} />,
    },
  ]

  return (
    <div className="space-y-6">
      <PageHeader title={t('incidents.title')} description={t('incidents.lead')} />
      <Card>
        <CardContent>
          {incidents.isError ? (
            <ErrorAlert error={incidents.error} />
          ) : (
            <DataTable
              label={t('incidents.title')}
              columns={columns}
              rows={incidents.data}
              rowKey={(i) => i.id}
              loading={incidents.isPending}
              empty={t('incidents.empty')}
              initialSort={{ id: 'severity', desc: false }}
              toolbar={
                <SwitchField
                  label={t('incidents.showResolved')}
                  checked={all}
                  onCheckedChange={(checked) => setAll(checked)}
                />
              }
            />
          )}
        </CardContent>
      </Card>
    </div>
  )
}
