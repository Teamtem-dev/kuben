import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDownIcon } from 'lucide-react'
import { useState } from 'react'
import { ErrorAlert, Loading, Section, Tag, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { when } from '@/lib/ops'
import { type Detached, detachedAppQuery, detachedQuery, releaseDetached } from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

interface InventoryItem {
  apiVersion?: string
  kind?: string
  name?: string
}

/** The objects a detached app left in its namespace, from its export. */
function Retained({ project, environment, id }: { project: string; environment: string; id: string }) {
  const { t } = usePrefs()
  const detail = useQuery(detachedAppQuery(project, environment, id))
  if (detail.error) return <ErrorAlert error={detail.error} />
  if (!detail.data) return <Loading />
  const exported = (detail.data.export ?? {}) as { inventory?: InventoryItem[]; runbook?: string[] }
  return (
    <div className="space-y-3 rounded-md bg-muted/50 p-3 text-sm">
      <h3 className="font-medium">{t('detached.retained')}</h3>
      <ul className="space-y-1">
        {(exported.inventory ?? []).map((item) => (
          <li key={`${item.kind}/${item.name}`} className="flex items-center gap-2">
            <Tag>{item.kind}</Tag>
            <span dir="ltr" className="font-mono text-xs">
              {item.name}
            </span>
          </li>
        ))}
      </ul>
      {exported.runbook && exported.runbook.length > 0 && (
        <>
          <h3 className="font-medium">{t('detached.runbook')}</h3>
          <ol className="list-decimal space-y-1 ps-5">
            {exported.runbook.map((step) => (
              <li key={step} dir="ltr" className="text-start font-mono text-xs">
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
    <li className="py-3 first:pt-0 last:pb-0">
      <Collapsible open={open} onOpenChange={setOpen} className="space-y-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 space-y-1 text-sm">
            <p className="flex flex-wrap items-center gap-2">
              <span dir="auto" className="font-medium">
                {app.app}
              </span>
              <ToneBadge
                tone={state === 'detaching' ? 'pending' : state === 'released' ? 'closed' : 'active'}
              >
                {t(`detached.state.${state}`)}
              </ToneBadge>
            </p>
            <p dir="auto">{app.reason}</p>
            <p className="text-muted-foreground text-xs">
              {when(app.requestedAt, locale)} · <span dir="ltr">{app.requestedBy}</span>
              {app.releasedAt && ` · ${t('detached.releasedAt')} ${when(app.releasedAt, locale)}`}
            </p>
          </div>
          <div className="flex flex-wrap gap-2">
            <CollapsibleTrigger asChild>
              <Button variant="outline" size="sm">
                <ChevronDownIcon
                  aria-hidden="true"
                  className={cn('transition-transform', open && 'rotate-180')}
                />
                {open ? t('detached.hide') : t('detached.show')}
              </Button>
            </CollapsibleTrigger>
            {state === 'detached' && (
              <Button size="sm" disabled={release.isPending} onClick={() => release.mutate()}>
                {t('detached.release')}
              </Button>
            )}
          </div>
        </div>
        {state === 'detached' && <p className="text-muted-foreground text-xs">{t('detached.releaseHint')}</p>}
        <ErrorAlert error={release.error} />
        <CollapsibleContent>
          <Retained project={project} environment={environment} id={app.id} />
        </CollapsibleContent>
      </Collapsible>
    </li>
  )
}

/** Apps detached from an environment (M4.11), shown when there are any. */
export function DetachedCard({ project, environment }: { project: string; environment: string }) {
  const { t } = usePrefs()
  const detached = useQuery(detachedQuery(project, environment))
  if (!detached.data || detached.data.length === 0) return <ErrorAlert error={detached.error} />
  return (
    <Section title={t('detached.title')}>
      <ul className="divide-y">
        {detached.data.map((d) => (
          <DetachedRow key={d.id} project={project} environment={environment} app={d} />
        ))}
      </ul>
    </Section>
  )
}
