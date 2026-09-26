import { useQuery } from '@tanstack/react-query'
import { getRouteApi, Link } from '@tanstack/react-router'
import {
  ArrowLeftIcon,
  CheckIcon,
  CircleHelpIcon,
  RefreshCwIcon,
  TriangleAlertIcon,
  XIcon,
} from 'lucide-react'
import { ErrorAlert, Loading, PageHeader, Section, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { type DoctorCheck, doctorQuery } from '@/lib/api'
import { type EvidenceNode, evidenceNodes } from '@/lib/evidence'
import type { Tone } from '@/lib/ops'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

const route = getRouteApi('/_authed/projects/$project/$environment/$app/doctor')

const statusTone: Record<string, Tone> = {
  ok: 'success',
  warn: 'warning',
  fail: 'danger',
  unknown: 'neutral',
}
const statusIcon = { ok: CheckIcon, warn: TriangleAlertIcon, fail: XIcon, unknown: CircleHelpIcon }
const dotColour: Record<string, string> = {
  ok: 'bg-success',
  warn: 'bg-warning',
  fail: 'bg-destructive',
  unknown: 'bg-muted-foreground',
}

/** A check's or a layer's status: a mark and a word, in the status's colour. */
function StatusChip({ status }: { status: string }) {
  const { tOr } = usePrefs()
  const Icon = statusIcon[status as keyof typeof statusIcon] ?? CircleHelpIcon
  return (
    <ToneBadge tone={statusTone[status] ?? 'neutral'}>
      <Icon aria-hidden="true" />
      {tOr(`doctor.status.${status}`, status)}
    </ToneBadge>
  )
}

/** One conclusion of the Doctor (`evidence.Finding`). */
interface Finding {
  kind: string
  layer: string
  status: string
  confidence: string
  summary: string
  evidence: string[]
  related: string[]
  action?: string | null
}

const kindTone: Record<string, Tone> = {
  rootCause: 'danger',
  symptom: 'warning',
  possibleCause: 'warning',
  unknown: 'warning',
}

function FindingRow({ finding }: { finding: Finding }) {
  const { t, tOr, locale } = usePrefs()
  return (
    <li className="space-y-2 py-3 text-sm first:pt-0 last:pb-0">
      <p className="flex flex-wrap items-center gap-2">
        <ToneBadge tone={kindTone[finding.kind] ?? 'warning'}>
          {tOr(`findings.kind.${finding.kind}`, finding.kind)}
        </ToneBadge>
        <span className="font-medium">{tOr(`findings.layer.${finding.layer}`, finding.layer)}</span>
        <span className="text-muted-foreground text-xs">
          {t('findings.confidence')} {tOr(`findings.confidence.${finding.confidence}`, finding.confidence)}
        </span>
      </p>
      <p dir="auto">{finding.summary}</p>
      {finding.evidence.length > 0 && (
        <ul className="list-disc space-y-0.5 ps-5 text-muted-foreground text-xs">
          {finding.evidence.map((e) => (
            <li key={e} dir="auto">
              {e}
            </li>
          ))}
        </ul>
      )}
      {finding.related.length > 0 && (
        <p className="text-muted-foreground text-xs">
          {t('findings.related')}{' '}
          {finding.related.map((l) => tOr(`findings.layer.${l}`, l)).join(locale === 'fa' ? '، ' : ', ')}
        </p>
      )}
      {finding.action && (
        <p dir="auto" className="text-muted-foreground text-xs">
          → {finding.action}
        </p>
      )}
    </li>
  )
}

/** The Doctor's conclusions, root causes first (M5.5). */
function Findings({ findings }: { findings: Finding[] }) {
  const { t } = usePrefs()
  if (findings.length === 0) return null
  return (
    <Section title={t('findings.title')}>
      <ul className="divide-y">
        {findings.map((f) => (
          <FindingRow key={`${f.kind}:${f.layer}:${f.summary}`} finding={f} />
        ))}
      </ul>
    </Section>
  )
}

const anchor = (layer: string) => `evidence-${layer}`

/**
 * One layer on the path: a dot in its status's colour on the line from the
 * build to the visitor, what it is, what was seen, what it depends on (links
 * to those layers) and what to do.
 */
function Layer({ node, root }: { node: EvidenceNode; root: boolean }) {
  const { t, tOr, locale } = usePrefs()
  const name = (layer: string) => tOr(`findings.layer.${layer}`, layer)
  return (
    <li id={anchor(node.layer)} className="relative scroll-mt-20 pb-5 ps-6 last:pb-0">
      <span
        aria-hidden="true"
        className={cn(
          '-start-[0.3125rem] absolute top-1.5 size-2.5 rounded-full ring-4 ring-card',
          dotColour[node.status] ?? dotColour.unknown,
        )}
      />
      <div className="space-y-1.5 text-sm">
        <h3 className="flex flex-wrap items-center gap-2 font-medium">
          {name(node.layer)}
          <StatusChip status={node.status} />
          {root && <ToneBadge tone="danger">{t('findings.kind.rootCause')}</ToneBadge>}
        </h3>
        {node.subject && (
          <p dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
            {node.subject}
          </p>
        )}
        {node.evidence.length > 0 ? (
          <ul className="list-disc space-y-0.5 ps-5 text-muted-foreground text-xs">
            {node.evidence.map((e) => (
              <li key={e} dir="auto">
                {e}
              </li>
            ))}
          </ul>
        ) : (
          <p className="text-muted-foreground text-xs">{t('evidence.nothing')}</p>
        )}
        {node.causes.length > 0 && (
          <p className="flex flex-wrap items-center gap-1.5 text-muted-foreground text-xs">
            {t('evidence.dependsOn')}
            {node.causes.map((c) => (
              <a key={c} href={`#${anchor(c)}`} className="text-link hover:underline">
                {name(c)}
              </a>
            ))}
          </p>
        )}
        {node.action && (
          <p dir="auto" className="text-muted-foreground text-xs">
            → {node.action}
          </p>
        )}
        {node.observedAt !== null && (
          <p className="text-muted-foreground text-xs">
            {t('evidence.observed')}{' '}
            <time dateTime={new Date(node.observedAt).toISOString()}>
              {new Date(node.observedAt).toLocaleString(locale)}
            </time>
          </p>
        )}
      </div>
    </li>
  )
}

/** The evidence graph as the path it is: upstream first, each layer with its causes. */
function EvidencePath({ nodes, roots }: { nodes: EvidenceNode[]; roots: Set<string> }) {
  const { t } = usePrefs()
  if (nodes.length === 0) return null
  return (
    <Section title={t('evidence.title')} description={t('evidence.lead')}>
      <ol aria-label={t('evidence.title')} className="ms-1 border-s">
        {nodes.map((node) => (
          <Layer key={node.layer} node={node} root={roots.has(node.layer)} />
        ))}
      </ol>
    </Section>
  )
}

function CheckRow({ check }: { check: DoctorCheck }) {
  const { tOr } = usePrefs()
  return (
    <li className="flex flex-col gap-1 py-3 first:pt-0 last:pb-0 sm:flex-row sm:items-start sm:gap-4">
      <div className="flex min-w-48 items-center gap-2">
        <StatusChip status={check.status} />
        <span className="font-medium text-sm">{tOr(`doctor.check.${check.id}`, check.id)}</span>
      </div>
      <div className="min-w-0 space-y-1 text-sm">
        {check.subject && (
          <p dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
            {check.subject}
          </p>
        )}
        <p dir="auto">{check.detail}</p>
        {check.hint && (
          <p dir="auto" className="text-muted-foreground text-xs">
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
  const findings = (report.data?.findings as unknown as Finding[] | undefined) ?? []
  const roots = new Set(findings.filter((f) => f.kind === 'rootCause').map((f) => f.layer))
  return (
    <div className="space-y-6">
      <PageHeader
        title={
          <>
            <span>
              {t('doctor.title')} · <span dir="auto">{app}</span>
            </span>
            {report.data && <StatusChip status={report.data.status} />}
          </>
        }
        description={t('doctor.lead')}
        actions={
          <>
            <Button variant="outline" asChild>
              <Link to="/projects/$project/$environment/$app" params={{ project, environment, app }}>
                <ArrowLeftIcon aria-hidden="true" className="rtl:-scale-x-100" />
                {t('doctor.back')}
              </Link>
            </Button>
            <Button variant="outline" disabled={report.isFetching} onClick={() => void report.refetch()}>
              <RefreshCwIcon aria-hidden="true" className={cn(report.isFetching && 'animate-spin')} />
              {report.isFetching ? t('doctor.checking') : t('doctor.again')}
            </Button>
          </>
        }
      />
      {report.isError ? (
        <ErrorAlert error={report.error} />
      ) : !report.data ? (
        <Loading>{t('doctor.checking')}</Loading>
      ) : (
        <>
          <Findings findings={findings} />
          <EvidencePath nodes={evidenceNodes(report.data.graph)} roots={roots} />
          <Section title={tOr(`doctor.summary.${report.data.status}`, report.data.status)}>
            <ul className="divide-y">
              {report.data.checks.map((check) => (
                <CheckRow key={`${check.id}:${check.subject}`} check={check} />
              ))}
            </ul>
          </Section>
        </>
      )}
    </div>
  )
}
