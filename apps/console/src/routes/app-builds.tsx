import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { ArrowLeftIcon, DownloadIcon } from 'lucide-react'
import type { ReactNode } from 'react'
import { type Column, DataTable } from '@/components/data-table'
import { ConfirmAction, Copyable, ErrorAlert, Loading, Section, Tag, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import {
  type Build,
  buildElapsed,
  buildTone,
  canCancelBuild,
  elapsed,
  imageDigest,
  sbomUrl,
  shortCommit,
  shortId,
} from '@/lib/delivery'
import { appScansQuery, buildQuery, buildsQuery, cancelBuild } from '@/lib/delivery-api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { BuildLog, BuildNow, StageTimeline } from './build-log'

type Where = { project: string; environment: string; app: string }

const appRoute = '/projects/$project/$environment/$app' as const

/** A build's phase as a coloured badge, and a stop that was asked for. */
function BuildStatus({ build }: { build: Build }) {
  const { t, tOr } = usePrefs()
  return (
    <span className="flex flex-wrap items-center gap-1">
      <ToneBadge tone={buildTone(build.phase)}>{tOr(`buildPhase.${build.phase}`, build.phase)}</ToneBadge>
      {build.cancelRequested && build.phase !== 'cancelRequested' && build.phase !== 'cancelled' && (
        <span className="text-muted-foreground text-xs">{t('builds.cancelRequested')}</span>
      )}
    </span>
  )
}

/** How long it ran, is running, or that it has not started. */
function BuildTiming({ build, now }: { build: Build; now: number }) {
  const { t } = usePrefs()
  const ms = buildElapsed(build, now)
  if (ms == null) return <>{t('builds.notStarted')}</>
  const duration = elapsed(ms)
  return <>{fill(t(build.finishedAt == null ? 'builds.running' : 'builds.took'), { duration })}</>
}

/** Why it failed or waits, and whether it was deployed. */
function BuildOutcome({ build }: { build: Build }) {
  const { tOr } = usePrefs()
  return (
    <div className="space-y-0.5">
      {build.failure && (
        <p className="text-destructive">{tOr(`buildFailure.${build.failure}`, build.failure)}</p>
      )}
      {build.failureDetail && (
        <p dir="auto" className="line-clamp-2 text-muted-foreground text-xs">
          {build.failureDetail}
        </p>
      )}
      {build.blockedReason && (
        <p dir="auto" className="text-warning text-xs">
          {build.blockedReason}
        </p>
      )}
      {build.deployDecision && (
        <p className="text-muted-foreground text-xs">
          {tOr(`buildDeploy.${build.deployDecision}`, build.deployDecision)}
        </p>
      )}
      {!build.failure && !build.blockedReason && !build.deployDecision && (
        <span className="text-muted-foreground">—</span>
      )}
    </div>
  )
}

/** Asks for a build to stop, after a confirmation; the dialog keeps the API's problem text. */
function CancelBuild({ project, environment, app, build }: Where & { build: Build }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const cancel = useMutation({
    mutationFn: () => cancelBuild(project, environment, app, build.id),
    onSuccess: (next) => {
      queryClient.setQueryData(buildQuery(project, environment, app, build.id).queryKey, next)
      return queryClient.invalidateQueries({ queryKey: ['app', project, environment, app, 'builds'] })
    },
  })
  return (
    <ConfirmAction
      label={t('builds.cancel')}
      title={fill(t('builds.cancelTitle'), { build: shortId(build.id) })}
      description={t('builds.cancelConfirm')}
      pending={cancel.isPending}
      error={cancel.error}
      onConfirm={() => cancel.mutateAsync()}
      variant="outline"
      size="sm"
    />
  )
}

/** The newest builds of the app: status, commit, timing, outcome, and a stop for running ones. */
function BuildList({ project, environment, app, repository }: Where & { repository: string }) {
  const { t, tOr, locale } = usePrefs()
  const builds = useQuery(buildsQuery(project, environment, app))
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const now = Date.now()
  const columns: Column<Build>[] = [
    {
      id: 'build',
      header: t('builds.build'),
      filterValue: (b) => b.id,
      className: 'max-w-48',
      cell: (b) => (
        <div className="min-w-0">
          <Link
            to={appRoute}
            params={{ project, environment, app }}
            search={{ tab: 'builds', build: b.id }}
            aria-label={fill(t('builds.detailsOf'), { build: b.id })}
            dir="ltr"
            className="block truncate text-start font-mono text-link text-xs hover:underline"
          >
            {shortId(b.id)}
          </Link>
          {b.attempt > 1 && (
            <span className="text-muted-foreground text-xs">
              {fill(t('builds.attempt'), { attempt: b.attempt })}
            </span>
          )}
        </div>
      ),
    },
    {
      id: 'status',
      header: t('builds.status'),
      sortValue: (b) => b.phase,
      filterValue: (b) => `${b.phase} ${tOr(`buildPhase.${b.phase}`, b.phase)}`,
      cell: (b) => <BuildStatus build={b} />,
    },
    {
      id: 'commit',
      header: t('builds.commit'),
      filterValue: (b) => `${b.commit} ${b.branch}`,
      cell: (b) => (
        <span dir="ltr" className="block text-start">
          <code className="font-mono text-xs">{shortCommit(b.commit)}</code>
          <span className="block text-muted-foreground text-xs">{b.branch}</span>
        </span>
      ),
    },
    {
      id: 'timing',
      header: t('builds.timing'),
      sortValue: (b) => b.createdAt,
      className: 'text-xs',
      cell: (b) => (
        <>
          <time dateTime={new Date(b.createdAt).toISOString()}>{date.format(b.createdAt)}</time>
          <span className="block text-muted-foreground">
            <BuildTiming build={b} now={now} />
          </span>
        </>
      ),
    },
    {
      id: 'outcome',
      header: t('builds.outcome'),
      filterValue: (b) =>
        [b.failure, b.failureDetail, b.blockedReason, b.deployDecision].filter(Boolean).join(' '),
      className: 'min-w-48 whitespace-normal text-sm',
      cell: (b) => <BuildOutcome build={b} />,
    },
    {
      id: 'action',
      header: t('builds.cancel'),
      hideHeader: true,
      className: 'text-end',
      cell: (b) =>
        canCancelBuild(b) ? (
          <CancelBuild project={project} environment={environment} app={app} build={b} />
        ) : null,
    },
  ]
  return (
    <Section
      title={t('builds.title')}
      description={fill(t('builds.lead'), { repository })}
      actions={<BuildNow project={project} environment={environment} app={app} />}
    >
      {builds.isError ? (
        <ErrorAlert error={builds.error} />
      ) : (
        <DataTable
          label={t('builds.title')}
          columns={columns}
          rows={builds.data}
          rowKey={(b) => b.id}
          loading={builds.isLoading}
          empty={t('builds.empty')}
          initialSort={{ id: 'timing', desc: true }}
          pageSize={10}
        />
      )}
    </Section>
  )
}

/** The scan of the build's image and its SBOM, while that image is in the app's current release. */
function BuildScan({ project, environment, app, digest }: Where & { digest: string }) {
  const { t } = usePrefs()
  const scans = useQuery(appScansQuery(project, environment, app))
  const image = scans.data?.images.find((i) => i.digest === digest)
  if (!scans.data || !image) return null
  const scan = image.scan
  return (
    <>
      <dt className="text-muted-foreground">{t('builds.scan')}</dt>
      <dd className="space-y-1">
        <p>
          <ToneBadge
            tone={scans.data.gate === 'pass' ? 'success' : scans.data.gate === 'block' ? 'danger' : 'warning'}
          >
            {fill(t('builds.gate'), { gate: scans.data.gate })}
          </ToneBadge>
        </p>
        <p className="text-xs">
          {!scan
            ? t('builds.notScanned')
            : scan.status === 'ok'
              ? fill(t('builds.scanCounts'), {
                  critical: scan.critical,
                  high: scan.high,
                  medium: scan.medium,
                  low: scan.low,
                })
              : t('builds.scanUnavailable')}
        </p>
        {image.sbom && (
          <Button variant="outline" size="sm" asChild>
            <a href={sbomUrl(project, environment, app, digest)} download={`sbom-${app}.cdx.json`}>
              <DownloadIcon aria-hidden="true" />
              {t('builds.sbom')}
            </a>
          </Button>
        )}
      </dd>
    </>
  )
}

function Row({ label, children }: { label: ReactNode; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  )
}

/** Every field the API gives about one build, its stages and its log. */
function BuildDetail({ project, environment, app, id }: Where & { id: string }) {
  const { t, tOr, locale } = usePrefs()
  const query = useQuery(buildQuery(project, environment, app, id))
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'medium' })
  const at = (ms: number | null | undefined) =>
    ms == null ? '—' : <time dateTime={new Date(ms).toISOString()}>{date.format(ms)}</time>
  const b = query.data
  const digest = imageDigest(b?.image)
  const now = Date.now()
  return (
    <div className="space-y-6">
      <Section
        title={fill(t('builds.detailTitle'), { build: shortId(id) })}
        actions={
          <>
            <Button variant="outline" size="sm" asChild>
              <Link to={appRoute} params={{ project, environment, app }} search={{ tab: 'builds' }}>
                <ArrowLeftIcon aria-hidden="true" className="rtl:-scale-x-100" />
                {t('builds.back')}
              </Link>
            </Button>
            {b && canCancelBuild(b) && (
              <CancelBuild project={project} environment={environment} app={app} build={b} />
            )}
          </>
        }
      >
        {query.isError ? (
          <ErrorAlert error={query.error} />
        ) : !b ? (
          <Loading lines={4} />
        ) : (
          <>
            <StageTimeline build={b} now={now} />
            <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-[max-content_1fr]">
              <Row label={t('builds.status')}>
                <BuildStatus build={b} />
              </Row>
              <Row label={t('builds.build')}>
                <span dir="ltr" className="block break-all text-start font-mono text-xs">
                  {b.id}
                </span>
                {b.attempt > 1 && (
                  <span className="text-muted-foreground text-xs">
                    {fill(t('builds.attempt'), { attempt: b.attempt })}
                  </span>
                )}
              </Row>
              <Row label={t('builds.repository')}>
                <span dir="ltr" className="block text-start font-mono text-xs">
                  {b.repository}
                </span>
              </Row>
              <Row label={t('builds.branch')}>
                <span dir="ltr" className="block text-start font-mono text-xs">
                  {b.branch}
                </span>
              </Row>
              <Row label={t('builds.commit')}>
                <span dir="ltr" className="block break-all text-start font-mono text-xs">
                  {b.commit}
                </span>
              </Row>
              <Row label={t('builds.strategy')}>
                <Tag>{b.strategy}</Tag>
              </Row>
              <Row label={t('builds.queued')}>{at(b.createdAt)}</Row>
              <Row label={t('builds.started')}>{at(b.startedAt)}</Row>
              <Row label={t('builds.finished')}>{at(b.finishedAt)}</Row>
              <Row label={t('builds.duration')}>
                <BuildTiming build={b} now={now} />
              </Row>
              {b.blockedReason && (
                <Row label={t('builds.blockedReason')}>
                  <span dir="auto" className="text-warning">
                    {b.blockedReason}
                  </span>
                </Row>
              )}
              {(b.failure || b.failureDetail) && (
                <Row label={t('builds.failure')}>
                  {b.failure && (
                    <p className="text-destructive">{tOr(`buildFailure.${b.failure}`, b.failure)}</p>
                  )}
                  {b.failureDetail && (
                    <pre
                      dir="ltr"
                      className="mt-1 max-h-64 overflow-auto whitespace-pre-wrap rounded-md bg-muted p-2 text-start font-mono text-xs"
                    >
                      {b.failureDetail}
                    </pre>
                  )}
                </Row>
              )}
              {b.image && (
                <Row label={t('builds.image')}>
                  <Copyable value={b.image} />
                </Row>
              )}
              {b.release && (
                <Row label={t('builds.release')}>
                  <span dir="ltr" className="block break-all text-start font-mono text-xs">
                    {b.release}
                  </span>
                </Row>
              )}
              {b.deployment && (
                <Row label={t('builds.deployment')}>
                  <Link
                    to={appRoute}
                    params={{ project, environment, app }}
                    search={{ tab: 'deployments' }}
                    dir="ltr"
                    className="block break-all text-start font-mono text-link text-xs hover:underline"
                  >
                    {b.deployment}
                  </Link>
                </Row>
              )}
              {b.deployDecision && (
                <Row label={t('builds.deployDecision')}>
                  {tOr(`buildDeploy.${b.deployDecision}`, b.deployDecision)}
                </Row>
              )}
              {digest && <BuildScan project={project} environment={environment} app={app} digest={digest} />}
            </dl>
          </>
        )}
      </Section>
      {b && <BuildLog key={b.id} project={project} environment={environment} app={app} build={b} />}
    </div>
  )
}

/** The builds tab of a Git-sourced app: the list, or one build when `?build=` names it. */
export function BuildsCard({
  project,
  environment,
  app,
  repository,
  build,
}: Where & { repository: string; build?: string }) {
  return build ? (
    <BuildDetail project={project} environment={environment} app={app} id={build} />
  ) : (
    <BuildList project={project} environment={environment} app={app} repository={repository} />
  )
}
