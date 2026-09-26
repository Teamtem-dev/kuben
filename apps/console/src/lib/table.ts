/**
 * The client-side work of the DataTable (components/data-table.tsx): text
 * filter, sort and pages over rows already fetched. Pure, so it is tested
 * without a browser.
 */

export type SortValue = string | number | null | undefined

export interface Sort {
  id: string
  desc: boolean
}

/** Folds case and Unicode forms so `É` finds `e`, `ك` finds `ک`, `ي` finds `ی`. */
export function normalize(text: string): string {
  return text
    .normalize('NFKD')
    .replace(/\p{M}/gu, '')
    .replace(/ك/g, 'ک')
    .replace(/[يى]/g, 'ی')
    .toLocaleLowerCase()
}

/** The rows whose text contains every word of `query` (any order, any column). */
export function filterRows<T>(rows: readonly T[], query: string, text: (row: T) => string): T[] {
  const words = normalize(query).split(/\s+/).filter(Boolean)
  if (words.length === 0) return [...rows]
  return rows.filter((row) => {
    const haystack = normalize(text(row))
    return words.every((w) => haystack.includes(w))
  })
}

/**
 * `rows` ordered by `value`, stable, with empty values last in either
 * direction; strings compare in `locale` with numeric runs as numbers.
 */
export function sortRows<T>(
  rows: readonly T[],
  value: (row: T) => SortValue,
  desc: boolean,
  locale: string,
): T[] {
  const collator = new Intl.Collator(locale, { numeric: true, sensitivity: 'base' })
  const dir = desc ? -1 : 1
  return rows
    .map((row, index) => ({ row, index, v: value(row) }))
    .sort((a, b) => {
      const aEmpty = a.v === null || a.v === undefined || a.v === ''
      const bEmpty = b.v === null || b.v === undefined || b.v === ''
      if (aEmpty || bEmpty) return aEmpty === bEmpty ? a.index - b.index : aEmpty ? 1 : -1
      const order =
        typeof a.v === 'number' && typeof b.v === 'number'
          ? a.v - b.v
          : collator.compare(String(a.v), String(b.v))
      return order === 0 ? a.index - b.index : order * dir
    })
    .map((x) => x.row)
}

/** The next sort after activating column `id`: ascending, descending, then none. */
export function nextSort(current: Sort | null, id: string): Sort | null {
  if (current?.id !== id) return { id, desc: false }
  if (!current.desc) return { id, desc: true }
  return null
}

export interface PageSlice<T> {
  rows: T[]
  /** 0-based, clamped into range. */
  page: number
  pages: number
  /** 1-based positions of the first and last row shown; 0 when there is none. */
  from: number
  to: number
}

/** One page of `rows`; a page past the end shows the last one. */
export function paginate<T>(rows: readonly T[], page: number, size: number): PageSlice<T> {
  const pages = Math.max(1, Math.ceil(rows.length / Math.max(1, size)))
  const current = Math.min(Math.max(0, Math.floor(page)), pages - 1)
  const start = current * size
  const slice = rows.slice(start, start + size)
  return {
    rows: slice,
    page: current,
    pages,
    from: slice.length ? start + 1 : 0,
    to: start + slice.length,
  }
}
