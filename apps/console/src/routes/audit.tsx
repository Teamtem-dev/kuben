import { useInfiniteQuery } from '@tanstack/react-query'
import { Button, Card, Empty, ErrorNote, PageHeader } from '../components/ui'
import { type AuditEvent, auditPage } from '../lib/api'
import { usePrefs } from '../lib/prefs'

const outcomeStyle: Record<string, string> = {
  success: 'text-ok',
  denied: 'text-danger',
  throttled: 'text-warn',
  failure: 'text-warn',
  error: 'text-danger',
}

function Row({ e }: { e: AuditEvent }) {
  const { tOr, locale } = usePrefs()
  return (
    <tr>
      <td className="whitespace-nowrap py-2 pe-4 text-muted">{new Date(e.at).toLocaleString(locale)}</td>
      <td dir="auto" className="py-2 pe-4">
        {e.actor ?? e.actor_kind}
      </td>
      <td dir="ltr" className="py-2 pe-4 text-start font-mono text-xs">
        {e.action}
      </td>
      <td dir="ltr" className="py-2 pe-4 text-start font-mono text-muted text-xs">
        {e.target ?? '—'}
      </td>
      <td className={`py-2 pe-4 ${outcomeStyle[e.outcome] ?? ''}`}>
        {tOr(`audit.outcome.${e.outcome}`, e.outcome)}
        {e.status ? ` (${e.status})` : ''}
      </td>
      <td dir="ltr" className="py-2 text-start font-mono text-subtle text-xs">
        {e.ip ?? '—'}
      </td>
    </tr>
  )
}

export function AuditPage() {
  const { t } = usePrefs()
  const log = useInfiniteQuery({
    queryKey: ['audit'],
    queryFn: ({ pageParam }) => auditPage(pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before ?? undefined,
    retry: false,
  })
  const events = log.data?.pages.flatMap((p) => p.events) ?? []

  return (
    <section className="space-y-6">
      <PageHeader title={t('audit.title')} subtitle={t('audit.lead')} />
      <Card>
        {log.isError ? (
          <ErrorNote error={log.error} />
        ) : events.length === 0 ? (
          <Empty>{log.isLoading ? t('common.loading') : t('audit.empty')}</Empty>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-start text-sm">
              <thead className="text-subtle text-xs">
                <tr>
                  <th className="pb-2 font-medium">{t('audit.when')}</th>
                  <th className="pb-2 font-medium">{t('audit.who')}</th>
                  <th className="pb-2 font-medium">{t('audit.action')}</th>
                  <th className="pb-2 font-medium">{t('audit.target')}</th>
                  <th className="pb-2 font-medium">{t('audit.outcome')}</th>
                  <th className="pb-2 font-medium">{t('audit.ip')}</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-line-soft">
                {events.map((e) => (
                  <Row key={e.id} e={e} />
                ))}
              </tbody>
            </table>
          </div>
        )}
        {log.hasNextPage && (
          <div className="mt-4">
            <Button variant="secondary" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
              {log.isFetchingNextPage ? t('common.loading') : t('audit.loadOlder')}
            </Button>
          </div>
        )}
      </Card>
    </section>
  )
}
