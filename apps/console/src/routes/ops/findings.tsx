import { Pill } from '../../components/ops'
import { Card } from '../../components/ui'
import { usePrefs } from '../../lib/prefs'

/** One conclusion of the Doctor (`kuben_platform::evidence::Finding`). */
export interface Finding {
  kind: string
  layer: string
  status: string
  confidence: string
  summary: string
  evidence: string[]
  related: string[]
  action?: string | null
}

const kindTone: Record<string, string> = {
  rootCause: 'failed',
  symptom: 'warning',
  possibleCause: 'warning',
  unknown: 'pending',
}

function FindingRow({ finding }: { finding: Finding }) {
  const { t, tOr, locale } = usePrefs()
  return (
    <li className="space-y-2 py-3 text-sm">
      <p className="flex flex-wrap items-center gap-2">
        <Pill tone={kindTone[finding.kind] ?? 'pending'}>
          {tOr(`findings.kind.${finding.kind}`, finding.kind)}
        </Pill>
        <span className="font-medium">{tOr(`findings.layer.${finding.layer}`, finding.layer)}</span>
        <span className="text-subtle text-xs">
          {t('findings.confidence')} {tOr(`findings.confidence.${finding.confidence}`, finding.confidence)}
        </span>
      </p>
      <p dir="auto" className="text-fg-soft">
        {finding.summary}
      </p>
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
        <p className="text-subtle text-xs">
          {t('findings.related')}{' '}
          {finding.related.map((l) => tOr(`findings.layer.${l}`, l)).join(locale === 'fa' ? '، ' : ', ')}
        </p>
      )}
      {finding.action && (
        <p dir="auto" className="text-subtle text-xs">
          → {finding.action}
        </p>
      )}
    </li>
  )
}

/** The Doctor's conclusions, root causes first (M5.5). */
export function FindingsCard({ findings }: { findings: Finding[] | undefined }) {
  const { t } = usePrefs()
  if (!findings || findings.length === 0) return null
  return (
    <Card title={t('findings.title')}>
      <ul className="divide-y divide-line-soft">
        {findings.map((f) => (
          <FindingRow key={`${f.kind}:${f.layer}:${f.summary}`} finding={f} />
        ))}
      </ul>
    </Card>
  )
}
