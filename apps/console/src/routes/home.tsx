/**
 * The home page: how things stand at a glance (projects, open incidents,
 * Kuben's own health), the latest deployments and every project. Only
 * existing endpoints: the projects, the open incidents, the health details
 * and — for the deployments, which have no organization-wide list — the
 * newest page of the audit log, where every deployment, rollback and
 * promotion is recorded.
 */
import { useQuery, useSuspenseQuery } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ActivityIcon, ArrowRightIcon, FolderKanbanIcon, type LucideIcon, SirenIcon } from 'lucide-react'
import type { ReactNode } from 'react'
import { EmptyState, ErrorAlert, Loading, Notice, PageHeader, Section, ToneBadge } from '@/components/kit'
import { Card, CardContent } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { healthQuery, projectsQuery, recentAuditQuery } from '@/lib/api'
import { DEPLOY_ACTIONS, recentDeployments, subsystemsOf } from '@/lib/home'
import { fill } from '@/lib/messages/pages'
import { incidentState, type Tone, when } from '@/lib/ops'
import { incidentsQuery } from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'
import { cn } from '@/lib/utils'
import { IncidentWhere } from './incidents'
import { NewProjectButton, ProjectCards } from './projects'

const SUBSYSTEM_TONES: Record<string, Tone> = {
  ok: 'success',
  degraded: 'danger',
  starting: 'warning',
  standby: 'neutral',
}

/** One number of the summary row, with what it counts and a line under it. */
function Stat({
  icon: Icon,
  label,
  value,
  detail,
  to,
  loading,
}: {
  icon: LucideIcon
  label: string
  value: ReactNode
  detail?: ReactNode
  to?: '/incidents'
  loading?: boolean
}) {
  const { t } = usePrefs()
  return (
    <Card className="gap-2 py-4">
      <CardContent className="space-y-1 px-4">
        <p className="flex items-center justify-between gap-2 text-muted-foreground text-sm">
          <span className="inline-flex items-center gap-2">
            <Icon aria-hidden="true" className="size-4" />
            {label}
          </span>
          {to && (
            <Link to={to} className="inline-flex items-center gap-1 text-link text-xs hover:underline">
              {t('home.viewAll')}
              <ArrowRightIcon aria-hidden="true" className="size-3 rtl:-scale-x-100" />
            </Link>
          )}
        </p>
        {loading ? (
          <div role="status" className="space-y-2 pt-1">
            <span className="sr-only">{t('common.loading')}</span>
            <Skeleton aria-hidden="true" className="h-7 w-16" />
            <Skeleton aria-hidden="true" className="h-3 w-24" />
          </div>
        ) : (
          <>
            <p className="font-semibold text-2xl tabular-nums">{value}</p>
            {detail && <p className="text-muted-foreground text-xs">{detail}</p>}
          </>
        )}
      </CardContent>
    </Card>
  )
}

function OpenIncidents() {
  const { t, tOr, locale } = usePrefs()
  const incidents = useQuery(incidentsQuery(false))
  const open = (incidents.data ?? []).filter((i) => incidentState(i) !== 'resolved').slice(0, 5)
  return (
    <Section
      title={t('home.openIncidents')}
      actions={
        <Link to="/incidents" className="inline-flex items-center gap-1 text-link text-sm hover:underline">
          {t('home.viewAll')}
          <ArrowRightIcon aria-hidden="true" className="size-3.5 rtl:-scale-x-100" />
        </Link>
      }
    >
      {incidents.isError ? (
        <ErrorAlert error={incidents.error} />
      ) : incidents.isPending ? (
        <Loading />
      ) : open.length === 0 ? (
        <EmptyState>{t('home.noIncidents')}</EmptyState>
      ) : (
        <ul className="divide-y">
          {open.map((i) => (
            <li key={i.id} className="space-y-1 py-3 first:pt-0 last:pb-0">
              <p className="flex flex-wrap items-center gap-2">
                <ToneBadge tone={i.severity}>{tOr(`incidents.severity.${i.severity}`, i.severity)}</ToneBadge>
                <span dir="auto" className="font-medium text-sm">
                  {i.title}
                </span>
              </p>
              <p className="flex flex-wrap items-center gap-x-3 text-muted-foreground text-xs">
                <time dateTime={i.openedAt}>{when(i.openedAt, locale)}</time>
                <IncidentWhere incident={i} />
              </p>
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

function RecentDeployments() {
  const { t, locale } = usePrefs()
  const audit = useQuery(recentAuditQuery)
  const runs = recentDeployments(audit.data?.events ?? [], 8)
  const forbidden = audit.error instanceof ApiError && audit.error.status === 403
  return (
    <Section title={t('home.recentDeployments')} description={t('home.recentDeploymentsHint')}>
      {forbidden ? (
        <Notice>{t('home.deploymentsForbidden')}</Notice>
      ) : audit.isError ? (
        <ErrorAlert error={audit.error} />
      ) : audit.isPending ? (
        <Loading />
      ) : runs.length === 0 ? (
        <EmptyState>{t('home.noDeployments')}</EmptyState>
      ) : (
        <ul className="divide-y">
          {runs.map((e) => {
            const [project, environment, app] = (e.target ?? '').split('/')
            return (
              <li
                key={e.id}
                className="flex flex-wrap items-center justify-between gap-2 py-3 first:pt-0 last:pb-0"
              >
                <div className="min-w-0 space-y-1">
                  <p className="flex flex-wrap items-center gap-2 text-sm">
                    <span className="font-medium">
                      {t(DEPLOY_ACTIONS[e.action] ?? 'home.deploy.startDeployment')}
                    </span>
                    {project && environment && app ? (
                      <Link
                        to="/projects/$project/$environment/$app"
                        params={{ project, environment, app }}
                        search={{ tab: 'deployments' }}
                        dir="ltr"
                        className="font-mono text-link text-xs hover:underline"
                      >
                        {e.target}
                      </Link>
                    ) : (
                      <span dir="ltr" className="font-mono text-xs">
                        {e.target ?? '—'}
                      </span>
                    )}
                  </p>
                  <p className="text-muted-foreground text-xs">
                    <time dateTime={new Date(e.at).toISOString()}>
                      {new Date(e.at).toLocaleString(locale)}
                    </time>
                    {e.actor && (
                      <>
                        {' · '}
                        <span dir="ltr">{e.actor}</span>
                      </>
                    )}
                  </p>
                </div>
              </li>
            )
          })}
        </ul>
      )}
    </Section>
  )
}

function PlatformHealth() {
  const { t, tOr } = usePrefs()
  const health = useQuery(healthQuery)
  const subsystems = subsystemsOf(health.data?.subsystems)
  return (
    <Section title={t('home.health')} description={t('home.healthHint')}>
      {health.isError ? (
        <ErrorAlert error={health.error} />
      ) : health.isPending ? (
        <Loading lines={2} />
      ) : subsystems.length === 0 ? (
        <EmptyState>{t('home.noSubsystems')}</EmptyState>
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
          {subsystems.map((s) => (
            <li key={s.name} className="space-y-1 rounded-lg border p-3">
              <p className="flex items-center justify-between gap-2">
                <span dir="ltr" className="truncate font-mono text-sm">
                  {s.name}
                </span>
                <ToneBadge tone={SUBSYSTEM_TONES[s.state] ?? 'neutral'}>
                  {tOr(`home.state.${s.state}`, s.state)}
                </ToneBadge>
              </p>
              {s.lastError && (
                <p dir="ltr" className="break-words text-start text-muted-foreground text-xs">
                  {s.lastError}
                </p>
              )}
            </li>
          ))}
        </ul>
      )}
    </Section>
  )
}

export function HomePage() {
  const { t } = usePrefs()
  const { data: projects } = useSuspenseQuery(projectsQuery)
  const incidents = useQuery(incidentsQuery(false))
  const health = useQuery(healthQuery)
  const open = (incidents.data ?? []).filter((i) => incidentState(i) !== 'resolved')
  const critical = open.filter((i) => i.severity === 'critical').length
  const ready = projects.filter((p) => p.ready).length

  return (
    <div className="space-y-6">
      <PageHeader title={t('nav.home')} description={t('home.lead')} />

      <div className="grid gap-4 sm:grid-cols-3">
        <Stat
          icon={FolderKanbanIcon}
          label={t('nav.projects')}
          value={projects.length}
          detail={fill(t('home.projectsReady'), { count: ready })}
        />
        <Stat
          icon={SirenIcon}
          label={t('home.openIncidents')}
          value={incidents.isError ? '—' : open.length}
          detail={incidents.isError ? undefined : fill(t('home.critical'), { count: critical })}
          to="/incidents"
          loading={incidents.isPending}
        />
        <Stat
          icon={ActivityIcon}
          label={t('home.health')}
          value={
            health.isError ? (
              '—'
            ) : (
              <span className={cn(health.data?.ready ? 'text-success' : 'text-destructive')}>
                {health.data?.ready ? t('home.healthy') : t('home.degraded')}
              </span>
            )
          }
          detail={
            health.data &&
            `${health.data.database} · ${health.data.cluster ? t('home.clusterConnected') : t('home.clusterMissing')}`
          }
          loading={health.isPending}
        />
      </div>

      <div className="grid gap-6 lg:grid-cols-2">
        <OpenIncidents />
        <RecentDeployments />
      </div>

      <section aria-labelledby="home-projects" className="space-y-4">
        <div className="flex flex-wrap items-center justify-between gap-3">
          <h2 id="home-projects" className="font-semibold text-lg">
            {t('nav.projects')}
          </h2>
          <NewProjectButton />
        </div>
        <ProjectCards projects={projects} />
      </section>

      <PlatformHealth />
    </div>
  )
}
