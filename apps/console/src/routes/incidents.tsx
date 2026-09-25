import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLinkIcon } from 'lucide-react'
import { useState } from 'react'
import { EmptyState, ErrorAlert, Loading, PageHeader, SwitchField, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { incidentState, when } from '@/lib/ops'
import { acknowledgeIncident, type Incident, incidentsQuery, resolveIncident } from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'

/** When the incident opened, was last seen, acknowledged and resolved, and by whom. */
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
    <ol className="space-y-1 border-s ps-3 text-muted-foreground text-xs">
      {rows.map(([label, at, by]) => (
        <li key={label}>
          <span className="font-medium text-foreground">{label}</span> · {at}
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
    <li className="space-y-3 py-4 first:pt-0 last:pb-0">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1.5">
          <p className="flex flex-wrap items-center gap-2">
            <ToneBadge tone={incident.severity}>
              {tOr(`incidents.severity.${incident.severity}`, incident.severity)}
            </ToneBadge>
            <ToneBadge tone={state}>{t(`incidents.state.${state}`)}</ToneBadge>
            <span dir="ltr" className="font-mono text-muted-foreground text-xs">
              {incident.kind}
            </span>
          </p>
          <h2 dir="auto" className="font-medium">
            {incident.title}
          </h2>
          {incident.detail && (
            <p dir="auto" className="text-muted-foreground text-sm">
              {incident.detail}
            </p>
          )}
          {incident.runbook && (
            <a
              href={incident.runbook}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex items-center gap-1 text-link text-sm hover:underline"
            >
              {t('incidents.runbook')}
              <ExternalLinkIcon aria-hidden="true" className="size-3.5" />
            </a>
          )}
        </div>
        {state !== 'resolved' && (
          <div className="flex gap-2">
            {state === 'open' && (
              <Button variant="outline" disabled={acknowledge.isPending} onClick={() => acknowledge.mutate()}>
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
      <ErrorAlert error={acknowledge.error ?? resolve.error} />
    </li>
  )
}

/** Incidents of the organization: open ones, or all. */
export function IncidentsPage() {
  const { t } = usePrefs()
  const [all, setAll] = useState(false)
  const incidents = useQuery(incidentsQuery(all))
  return (
    <div className="space-y-6">
      <PageHeader
        title={t('incidents.title')}
        description={t('incidents.lead')}
        actions={
          <SwitchField
            label={t('incidents.showResolved')}
            checked={all}
            onCheckedChange={(checked) => setAll(checked)}
          />
        }
      />
      <ErrorAlert error={incidents.error} />
      {incidents.isPending && <Loading />}
      {incidents.data && incidents.data.length === 0 && <EmptyState>{t('incidents.empty')}</EmptyState>}
      {incidents.data && incidents.data.length > 0 && (
        <Card>
          <CardContent>
            <ul className="divide-y">
              {incidents.data.map((i) => (
                <IncidentRow key={i.id} incident={i} />
              ))}
            </ul>
          </CardContent>
        </Card>
      )}
    </div>
  )
}
