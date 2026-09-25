import { useInfiniteQuery } from '@tanstack/react-query'
import { type Column, DataTable } from '@/components/data-table'
import { ErrorAlert, PageHeader } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { type AuditEvent, auditPage } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'

const outcomeStyle: Record<string, string> = {
  success: 'text-success',
  denied: 'text-destructive',
  throttled: 'text-warning',
  failure: 'text-warning',
  error: 'text-destructive',
}

export function AuditPage() {
  const { t, tOr, locale } = usePrefs()
  const log = useInfiniteQuery({
    queryKey: ['audit'],
    queryFn: ({ pageParam }) => auditPage(pageParam),
    initialPageParam: undefined as number | undefined,
    getNextPageParam: (last) => last.next_before ?? undefined,
    retry: false,
  })
  const events = log.data?.pages.flatMap((p) => p.events)
  const outcome = (e: AuditEvent) => tOr(`audit.outcome.${e.outcome}`, e.outcome)

  const columns: Column<AuditEvent>[] = [
    {
      id: 'when',
      header: t('audit.when'),
      sortValue: (e) => e.at,
      className: 'text-muted-foreground',
      cell: (e) => (
        <time dateTime={new Date(e.at).toISOString()}>{new Date(e.at).toLocaleString(locale)}</time>
      ),
    },
    {
      id: 'who',
      header: t('audit.who'),
      sortValue: (e) => e.actor ?? e.actor_kind,
      filterValue: (e) => e.actor ?? e.actor_kind,
      cell: (e) => <span dir="auto">{e.actor ?? e.actor_kind}</span>,
    },
    {
      id: 'action',
      header: t('audit.action'),
      sortValue: (e) => e.action,
      filterValue: (e) => e.action,
      cell: (e) => (
        <span dir="ltr" className="font-mono text-xs">
          {e.action}
        </span>
      ),
    },
    {
      id: 'target',
      header: t('audit.target'),
      sortValue: (e) => e.target,
      filterValue: (e) => e.target,
      cell: (e) => (
        <span dir="ltr" className="font-mono text-muted-foreground text-xs">
          {e.target ?? '—'}
        </span>
      ),
    },
    {
      id: 'outcome',
      header: t('audit.outcome'),
      sortValue: outcome,
      filterValue: (e) => `${outcome(e)} ${e.outcome} ${e.status ?? ''}`,
      cell: (e) => (
        <span className={outcomeStyle[e.outcome]}>
          {outcome(e)}
          {e.status ? ` (${e.status})` : ''}
        </span>
      ),
    },
    {
      id: 'ip',
      header: t('audit.ip'),
      filterValue: (e) => e.ip,
      cell: (e) => (
        <span dir="ltr" className="font-mono text-muted-foreground text-xs">
          {e.ip ?? '—'}
        </span>
      ),
    },
  ]

  return (
    <div className="space-y-6">
      <PageHeader title={t('audit.title')} description={t('audit.lead')} />
      <Card>
        <CardContent className="space-y-4">
          {log.isError ? (
            <ErrorAlert error={log.error} />
          ) : (
            <DataTable
              label={t('audit.title')}
              columns={columns}
              rows={events}
              rowKey={(e) => e.id}
              loading={log.isLoading}
              empty={t('audit.empty')}
              initialSort={{ id: 'when', desc: true }}
              footer={
                log.hasNextPage && (
                  <div>
                    <Button
                      variant="outline"
                      disabled={log.isFetchingNextPage}
                      onClick={() => log.fetchNextPage()}
                    >
                      {log.isFetchingNextPage ? t('common.loading') : t('audit.loadOlder')}
                    </Button>
                  </div>
                )
              }
            />
          )}
        </CardContent>
      </Card>
    </div>
  )
}
