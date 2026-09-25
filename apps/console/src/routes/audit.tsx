import { useInfiniteQuery } from '@tanstack/react-query'
import { EmptyState, ErrorAlert, Loading, PageHeader } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { type AuditEvent, auditPage } from '@/lib/api'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

const outcomeStyle: Record<string, string> = {
  success: 'text-success',
  denied: 'text-destructive',
  throttled: 'text-warning',
  failure: 'text-warning',
  error: 'text-destructive',
}

function Row({ e }: { e: AuditEvent }) {
  const { tOr, locale } = usePrefs()
  return (
    <TableRow>
      <TableCell className="text-muted-foreground">
        <time dateTime={new Date(e.at).toISOString()}>{new Date(e.at).toLocaleString(locale)}</time>
      </TableCell>
      <TableCell dir="auto">{e.actor ?? e.actor_kind}</TableCell>
      <TableCell dir="ltr" className="text-start font-mono text-xs">
        {e.action}
      </TableCell>
      <TableCell dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
        {e.target ?? '—'}
      </TableCell>
      <TableCell className={cn(outcomeStyle[e.outcome])}>
        {tOr(`audit.outcome.${e.outcome}`, e.outcome)}
        {e.status ? ` (${e.status})` : ''}
      </TableCell>
      <TableCell dir="ltr" className="text-start font-mono text-muted-foreground text-xs">
        {e.ip ?? '—'}
      </TableCell>
    </TableRow>
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
    <div className="space-y-6">
      <PageHeader title={t('audit.title')} description={t('audit.lead')} />
      <Card>
        <CardContent className="space-y-4">
          {log.isError ? (
            <ErrorAlert error={log.error} />
          ) : log.isLoading ? (
            <Loading />
          ) : events.length === 0 ? (
            <EmptyState>{t('audit.empty')}</EmptyState>
          ) : (
            <Table>
              <TableHeader>
                <TableRow>
                  <TableHead>{t('audit.when')}</TableHead>
                  <TableHead>{t('audit.who')}</TableHead>
                  <TableHead>{t('audit.action')}</TableHead>
                  <TableHead>{t('audit.target')}</TableHead>
                  <TableHead>{t('audit.outcome')}</TableHead>
                  <TableHead>{t('audit.ip')}</TableHead>
                </TableRow>
              </TableHeader>
              <TableBody>
                {events.map((e) => (
                  <Row key={e.id} e={e} />
                ))}
              </TableBody>
            </Table>
          )}
          {log.hasNextPage && (
            <div>
              <Button variant="outline" disabled={log.isFetchingNextPage} onClick={() => log.fetchNextPage()}>
                {log.isFetchingNextPage ? t('common.loading') : t('audit.loadOlder')}
              </Button>
            </div>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
