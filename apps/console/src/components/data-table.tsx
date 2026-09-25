/**
 * A table of rows the page already has: a text filter, sortable columns and
 * pages, all in the browser. Sorting is a button in each sortable header
 * (the header carries `aria-sort`); the filter is a search box; the pager is
 * a labelled navigation with previous/next buttons and a page size. Changes
 * are announced politely (the count line is a live region).
 */
import { ArrowDownIcon, ArrowUpDownIcon, ArrowUpIcon, ChevronLeftIcon, ChevronRightIcon } from 'lucide-react'
import { type ReactNode, useId, useMemo, useState } from 'react'
import { EmptyState, TableSkeleton } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { filterRows, nextSort, paginate, type Sort, type SortValue, sortRows } from '@/lib/table'
import { cn } from '@/lib/utils'

export interface Column<T> {
  id: string
  /** The header; with `hideHeader` it is read by screen readers only. */
  header: ReactNode
  hideHeader?: boolean
  cell: (row: T) => ReactNode
  /** Makes the column sortable by this value. */
  sortValue?: (row: T) => SortValue
  /** Text of the row this column adds to the filter. */
  filterValue?: (row: T) => string | null | undefined
  className?: string
}

const PAGE_SIZES = [10, 25, 50, 100] as const

export function DataTable<T>({
  label,
  columns,
  rows,
  rowKey,
  loading = false,
  empty,
  initialSort = null,
  pageSize: initialPageSize = 25,
  toolbar,
  footer,
}: {
  /** Names the table (its caption) and the filter box. */
  label: string
  columns: readonly Column<T>[]
  rows: readonly T[] | undefined
  rowKey: (row: T) => string
  loading?: boolean
  /** Shown instead of the table when there are no rows at all. */
  empty: ReactNode
  initialSort?: Sort | null
  pageSize?: number
  /** More controls on the filter's line (a switch, a button). */
  toolbar?: ReactNode
  /** Below the pager (e.g. "load older" for a paged API). */
  footer?: ReactNode
}) {
  const { t, locale } = usePrefs()
  const [query, setQuery] = useState('')
  const [sort, setSort] = useState<Sort | null>(initialSort)
  const [page, setPage] = useState(0)
  const [pageSize, setPageSize] = useState(initialPageSize)
  const filterId = useId()
  const sizeId = useId()

  const filterable = columns.some((c) => c.filterValue)
  const shown = useMemo(() => {
    const all = rows ?? []
    const text = (row: T) =>
      columns
        .map((c) => c.filterValue?.(row) ?? '')
        .filter(Boolean)
        .join(' ')
    const filtered = filterable ? filterRows(all, query, text) : [...all]
    const column = sort && columns.find((c) => c.id === sort.id)
    return column?.sortValue && sort ? sortRows(filtered, column.sortValue, sort.desc, locale) : filtered
  }, [rows, columns, filterable, query, sort, locale])
  const slice = paginate(shown, page, pageSize)

  if (loading) {
    return (
      <div className="space-y-4">
        {toolbar && <div className="flex flex-wrap items-center justify-end gap-3">{toolbar}</div>}
        <TableSkeleton columns={columns.length} />
      </div>
    )
  }
  if (!rows || rows.length === 0) {
    return (
      <div className="space-y-4">
        {toolbar && <div className="flex flex-wrap items-center justify-end gap-3">{toolbar}</div>}
        <EmptyState>{empty}</EmptyState>
        {footer}
      </div>
    )
  }

  const total = rows.length
  const count =
    shown.length === total
      ? fill(t('table.count'), { count: total })
      : fill(t('table.countFiltered'), { shown: shown.length, count: total })

  return (
    <div className="space-y-3">
      {(filterable || toolbar) && (
        <div className="flex flex-wrap items-center justify-between gap-3">
          {filterable && (
            <div className="w-full sm:w-72">
              <label htmlFor={filterId} className="sr-only">
                {fill(t('table.filterLabel'), { what: label })}
              </label>
              <Input
                id={filterId}
                type="search"
                dir="auto"
                value={query}
                placeholder={t('table.filter')}
                onChange={(e) => {
                  setQuery(e.target.value)
                  setPage(0)
                }}
              />
            </div>
          )}
          {toolbar && <div className="flex flex-wrap items-center gap-3">{toolbar}</div>}
        </div>
      )}

      <Table>
        <caption className="sr-only">{label}</caption>
        <TableHeader>
          <TableRow>
            {columns.map((c) => {
              const sorted = sort?.id === c.id ? sort : null
              const Icon = !sorted ? ArrowUpDownIcon : sorted.desc ? ArrowDownIcon : ArrowUpIcon
              return (
                <TableHead
                  key={c.id}
                  scope="col"
                  aria-sort={sorted ? (sorted.desc ? 'descending' : 'ascending') : undefined}
                  className={cn(c.sortValue && 'px-0')}
                >
                  {c.hideHeader ? (
                    <span className="sr-only">{c.header}</span>
                  ) : c.sortValue ? (
                    <Button
                      type="button"
                      variant="ghost"
                      size="sm"
                      className="h-8 px-2 font-medium"
                      onClick={() => {
                        setSort((s) => nextSort(s, c.id))
                        setPage(0)
                      }}
                    >
                      {c.header}
                      <Icon
                        aria-hidden="true"
                        className={cn('size-3.5', !sorted && 'text-muted-foreground')}
                      />
                    </Button>
                  ) : (
                    c.header
                  )}
                </TableHead>
              )
            })}
          </TableRow>
        </TableHeader>
        <TableBody>
          {slice.rows.length === 0 ? (
            <TableRow>
              <TableCell colSpan={columns.length} className="h-20 text-center text-muted-foreground">
                {t('table.noMatch')}
              </TableCell>
            </TableRow>
          ) : (
            slice.rows.map((row) => (
              <TableRow key={rowKey(row)}>
                {columns.map((c) => (
                  <TableCell key={c.id} className={c.className}>
                    {c.cell(row)}
                  </TableCell>
                ))}
              </TableRow>
            ))
          )}
        </TableBody>
      </Table>

      <div className="flex flex-wrap items-center justify-between gap-3 text-muted-foreground text-sm">
        <p aria-live="polite">{count}</p>
        {shown.length > PAGE_SIZES[0] && (
          <nav aria-label={fill(t('table.pages'), { what: label })} className="flex items-center gap-2">
            <label htmlFor={sizeId} className="sr-only">
              {t('table.pageSize')}
            </label>
            <NativeSelect
              id={sizeId}
              size="sm"
              value={pageSize}
              onChange={(e) => {
                setPageSize(Number(e.target.value))
                setPage(0)
              }}
            >
              {PAGE_SIZES.map((n) => (
                <option key={n} value={n}>
                  {fill(t('table.perPage'), { count: n })}
                </option>
              ))}
            </NativeSelect>
            <span className="whitespace-nowrap">
              {fill(t('table.page'), { page: slice.page + 1, pages: slice.pages })}
            </span>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label={t('table.previous')}
              title={t('table.previous')}
              disabled={slice.page === 0}
              onClick={() => setPage(slice.page - 1)}
            >
              <ChevronLeftIcon aria-hidden="true" className="rtl:-scale-x-100" />
            </Button>
            <Button
              type="button"
              variant="outline"
              size="icon-sm"
              aria-label={t('table.next')}
              title={t('table.next')}
              disabled={slice.page >= slice.pages - 1}
              onClick={() => setPage(slice.page + 1)}
            >
              <ChevronRightIcon aria-hidden="true" className="rtl:-scale-x-100" />
            </Button>
          </nav>
        )}
      </div>
      {footer}
    </div>
  )
}
