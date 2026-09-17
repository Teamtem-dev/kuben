import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useState } from 'react'
import { Pill } from '../../components/ops'
import { Badge, Button, Card, ErrorNote } from '../../components/ui'
import { when } from '../../lib/ops'
import { type Detached, detachedAppQuery, detachedQuery, releaseDetached } from '../../lib/ops-api'
import { usePrefs } from '../../lib/prefs'

interface InventoryItem {
  apiVersion?: string
  kind?: string
  name?: string
}

/** The objects a detached app left in its namespace, from its export. */
function Retained({ project, environment, id }: { project: string; environment: string; id: string }) {
  const { t } = usePrefs()
  const detail = useQuery(detachedAppQuery(project, environment, id))
  if (detail.error) return <ErrorNote error={detail.error} />
  if (!detail.data) return <p className="text-subtle text-sm">{t('common.loading')}</p>
  const exported = (detail.data.export ?? {}) as { inventory?: InventoryItem[]; runbook?: string[] }
  return (
    <div className="space-y-3 rounded-lg bg-inset p-3 text-sm">
      <h3 className="font-medium">{t('detached.retained')}</h3>
      <ul className="space-y-1">
        {(exported.inventory ?? []).map((item) => (
          <li key={`${item.kind}/${item.name}`} className="flex items-center gap-2">
            <Badge>{item.kind}</Badge>
            <span dir="ltr" className="font-mono text-xs">
              {item.name}
            </span>
          </li>
        ))}
      </ul>
      {exported.runbook && exported.runbook.length > 0 && (
        <>
          <h3 className="font-medium">{t('detached.runbook')}</h3>
          <ol className="list-decimal space-y-1 ps-5 text-fg-soft">
            {exported.runbook.map((step) => (
              <li key={step} dir="ltr" className="text-start">
                {step}
              </li>
            ))}
          </ol>
        </>
      )}
    </div>
  )
}

function DetachedRow({ project, environment, app }: { project: string; environment: string; app: Detached }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const release = useMutation({
    mutationFn: () => releaseDetached(project, environment, app.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['detached', project, environment] }),
  })
  const state = app.releasedAt ? 'released' : app.completedAt ? 'detached' : 'detaching'
  return (
    <li className="space-y-3 py-3">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1 text-sm">
          <p className="flex flex-wrap items-center gap-2">
            <span className="font-medium">{app.app}</span>
            <Pill tone={state === 'detaching' ? 'pending' : state === 'released' ? 'closed' : 'active'}>
              {t(`detached.state.${state}`)}
            </Pill>
          </p>
          <p dir="auto" className="text-fg-soft">
            {app.reason}
          </p>
          <p className="text-subtle text-xs">
            {when(app.requestedAt, locale)} · <span dir="ltr">{app.requestedBy}</span>
            {app.releasedAt && ` · ${t('detached.releasedAt')} ${when(app.releasedAt, locale)}`}
          </p>
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
            {open ? t('detached.hide') : t('detached.show')}
          </Button>
          {state === 'detached' && (
            <Button disabled={release.isPending} onClick={() => release.mutate()}>
              {t('detached.release')}
            </Button>
          )}
        </div>
      </div>
      {state === 'detached' && <p className="text-subtle text-xs">{t('detached.releaseHint')}</p>}
      <ErrorNote error={release.error} />
      {open && <Retained project={project} environment={environment} id={app.id} />}
    </li>
  )
}

/** Apps detached from an environment (M4.11), shown when there are any. */
export function DetachedCard({ project, environment }: { project: string; environment: string }) {
  const { t } = usePrefs()
  const detached = useQuery(detachedQuery(project, environment))
  if (!detached.data || detached.data.length === 0) return <ErrorNote error={detached.error} />
  return (
    <Card title={t('detached.title')}>
      <ul className="divide-y divide-line-soft">
        {detached.data.map((d) => (
          <DetachedRow key={d.id} project={project} environment={environment} app={d} />
        ))}
      </ul>
    </Card>
  )
}
