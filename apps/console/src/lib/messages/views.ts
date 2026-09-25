/** Strings of the F3 views: the DataTable, the home dashboard, controls and settings. */
export const viewsEn = {
  'table.filter': 'Filter…',
  'table.filterLabel': 'Filter {what}',
  'table.count': '{count} rows',
  'table.countFiltered': '{shown} of {count} rows',
  'table.noMatch': 'Nothing matches the filter.',
  'table.pages': '{what}: pages',
  'table.page': 'Page {page} of {pages}',
  'table.pageSize': 'Rows per page',
  'table.perPage': '{count} per page',
  'table.previous': 'Previous page',
  'table.next': 'Next page',
} as const

export const viewsFa: Record<keyof typeof viewsEn, string> = {
  'table.filter': 'فیلتر…',
  'table.filterLabel': 'فیلتر {what}',
  'table.count': '{count} ردیف',
  'table.countFiltered': '{shown} از {count} ردیف',
  'table.noMatch': 'هیچ ردیفی با فیلتر جور نیست.',
  'table.pages': '{what}: صفحه‌ها',
  'table.page': 'صفحهٔ {page} از {pages}',
  'table.pageSize': 'ردیف در هر صفحه',
  'table.perPage': '{count} در هر صفحه',
  'table.previous': 'صفحهٔ قبل',
  'table.next': 'صفحهٔ بعد',
}
