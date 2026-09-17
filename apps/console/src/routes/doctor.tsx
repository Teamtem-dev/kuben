import { useQuery } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import { Button, Card, ErrorNote, PageHeader } from '../components/ui'
import { type DoctorCheck, doctorQuery } from '../lib/api'
import { usePrefs } from '../lib/prefs'
import { type Finding, FindingsCard } from './ops/findings'

const route = getRouteApi('/_authed/projects/$project/$environment/$app/doctor')

const tones: Record<string, string> = {
  ok: 'border-ok/30 bg-ok/5 text-ok',
  warn: 'border-warn/30 bg-warn/10 text-warn',
  fail: 'border-danger/30 bg-danger/10 text-danger',
  unknown: 'border-line-strong bg-hover text-muted',
}

const marks: Record<string, string> = { ok: '✓', warn: '!', fail: '✕', unknown: '?' }

function StatusChip({ status }: { status: string }) {
  const { tOr } = usePrefs()
  return (
    <span
      className={`inline-flex shrink-0 items-center gap-1 rounded-full border px-2 py-0.5 font-medium text-xs ${tones[status] ?? tones.unknown}`}
    >
      <span aria-hidden="true">{marks[status] ?? '?'}</span>
      {tOr(`doctor.status.${status}`, status)}
    </span>
  )
}

function CheckRow({ check }: { check: DoctorCheck }) {
  const { tOr } = usePrefs()
  return (
    <li className="flex flex-col gap-1 py-3 sm:flex-row sm:items-start sm:gap-4">
      <div className="flex min-w-48 items-center gap-2">
        <StatusChip status={check.status} />
        <span className="font-medium text-sm">{tOr(`doctor.check.${check.id}`, check.id)}</span>
      </div>
      <div className="min-w-0 space-y-1 text-sm">
        {check.subject && (
          <p dir="ltr" className="text-start font-mono text-muted text-xs">
            {check.subject}
          </p>
        )}
        <p dir="auto" className="text-fg-soft">
          {check.detail}
        </p>
        {check.hint && (
          <p dir="auto" className="text-subtle text-xs">
            → {check.hint}
          </p>
        )}
      </div>
    </li>
  )
}

export function DoctorPage() {
  const { project, environment, app } = route.useParams()
  const { t, tOr } = usePrefs()
  const report = useQuery({ ...doctorQuery(project, environment, app), retry: false })
  return (
    <section className="space-y-6">
      <PageHeader
        crumbs={
          <Link
            to="/projects/$project/$environment/$app"
            params={{ project, environment, app }}
            className="hover:text-fg"
          >
            {t('doctor.back')}
          </Link>
        }
        title={
          <span className="flex items-center gap-3">
            {t('doctor.title')} · {app}
            {report.data && <StatusChip status={report.data.status} />}
          </span>
        }
        subtitle={t('doctor.lead')}
        actions={
          <Button variant="secondary" disabled={report.isFetching} onClick={() => void report.refetch()}>
            {report.isFetching ? t('doctor.checking') : t('doctor.again')}
          </Button>
        }
      />
      {report.isError ? (
        <ErrorNote error={report.error} />
      ) : !report.data ? (
        <p className="text-subtle text-sm">{t('doctor.checking')}</p>
      ) : (
        <>
          <FindingsCard findings={report.data.findings as unknown as Finding[] | undefined} />
          <Card title={tOr(`doctor.summary.${report.data.status}`, report.data.status)}>
            <ul className="divide-y divide-line-soft">
              {report.data.checks.map((check) => (
                <CheckRow key={`${check.id}:${check.subject}`} check={check} />
              ))}
            </ul>
          </Card>
        </>
      )}
    </section>
  )
}
