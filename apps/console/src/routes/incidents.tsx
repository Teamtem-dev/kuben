import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Pill } from '../components/ops'
import { Button, Card, Empty, ErrorNote, PageHeader } from '../components/ui'
import { incidentState, when } from '../lib/ops'
import { acknowledgeIncident, type Incident, incidentsQuery, resolveIncident } from '../lib/ops-api'
import { usePrefs } from '../lib/prefs'

function Timeline({ incident }: { incident: Incident }) {
  const { t, locale } = usePrefs()
  const rows: [string, string, string | null | undefined][] = [
    [t('incidents.opened'), when(incident.openedAt, locale), null],
    [
      t('incidents.lastSeen'),
      `${when(incident.lastSeenAt, locale)} · ${t('incidents.occurrences')} ${incident.occurrences}`,
      null,
    ],
  ]
  if (incident.acknowledgedAt) {
    rows.push([t('incidents.acknowledged'), when(incident.acknowledgedAt, locale), incident.acknowledgedBy])
  }
  if (incident.resolvedAt) {
    rows.push([t('incidents.resolved'), when(incident.resolvedAt, locale), incident.resolvedBy])
  }
  return (
    <ol className="space-y-1 border-line border-s ps-3 text-xs">
      {rows.map(([label, at, by]) => (
        <li key={label} className="text-muted-foreground">
          <span className="font-medium text-fg-soft">{label}</span> · {at}
          {by && (
            <>
              {' '}
              · <span dir="ltr">{by}</span>
            </>
          )}
        </li>
      ))}
    </ol>
  )
}

function IncidentRow({ incident }: { incident: Incident }) {
  const { t, tOr } = usePrefs()
  const queryClient = useQueryClient()
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['incidents'] })
  const acknowledge = useMutation({ mutationFn: () => acknowledgeIncident(incident.id), onSuccess: refresh })
  const resolve = useMutation({ mutationFn: () => resolveIncident(incident.id), onSuccess: refresh })
  const state = incidentState(incident)
  return (
    <li className="space-y-3 py-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <p className="flex flex-wrap items-center gap-2">
            <Pill tone={incident.severity}>
              {tOr(`incidents.severity.${incident.severity}`, incident.severity)}
            </Pill>
            <Pill tone={state}>{t(`incidents.state.${state}`)}</Pill>
            <span dir="ltr" className="font-mono text-subtle text-xs">
              {incident.kind}
            </span>
          </p>
          <h2 dir="auto" className="font-medium">
            {incident.title}
          </h2>
          {incident.detail && (
            <p dir="auto" className="text-fg-soft text-sm">
              {incident.detail}
            </p>
          )}
          {incident.runbook && (
            <a
              href={incident.runbook}
              target="_blank"
              rel="noopener noreferrer"
              className="text-link text-sm hover:underline"
            >
              {t('incidents.runbook')}
            </a>
          )}
        </div>
        {state !== 'resolved' && (
          <div className="flex gap-2">
            {state === 'open' && (
              <Button
                variant="secondary"
                disabled={acknowledge.isPending}
                onClick={() => acknowledge.mutate()}
              >
                {t('incidents.acknowledge')}
              </Button>
            )}
            <Button disabled={resolve.isPending} onClick={() => resolve.mutate()}>
              {t('incidents.resolve')}
            </Button>
          </div>
        )}
      </div>
      <Timeline incident={incident} />
      <ErrorNote error={acknowledge.error ?? resolve.error} />
    </li>
  )
}

/** Incidents of the organization: open ones, or all. */
export function IncidentsPage() {
  const { t } = usePrefs()
  const [all, setAll] = useState(false)
  const incidents = useQuery(incidentsQuery(all))
  return (
    <section className="space-y-6">
      <PageHeader
        title={t('incidents.title')}
        subtitle={t('incidents.lead')}
        actions={
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={all} onChange={(e) => setAll(e.target.checked)} />
            {t('incidents.showResolved')}
          </label>
        }
      />
      <ErrorNote error={incidents.error} />
      {incidents.isPending && <p className="text-subtle text-sm">{t('common.loading')}</p>}
      {incidents.data && incidents.data.length === 0 && <Empty>{t('incidents.empty')}</Empty>}
      {incidents.data && incidents.data.length > 0 && (
        <Card>
          <ul className="divide-y divide-line-soft">
            {incidents.data.map((i) => (
              <IncidentRow key={i.id} incident={i} />
            ))}
          </ul>
        </Card>
      )}
    </section>
  )
}
