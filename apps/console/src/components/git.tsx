/**
 * Git building blocks shared by the integrations page and the app's
 * "connect a repository" dialog: a provider's mark, the outcome of a token
 * check, a repository browser and a branch picker.
 */
import { useQuery } from '@tanstack/react-query'
import { ChevronLeftIcon, ChevronRightIcon, GitBranchIcon, LockIcon } from 'lucide-react'
import { useEffect, useId, useState } from 'react'
import { ErrorAlert, Loading, SelectInput, Tag, ToneBadge } from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import {
  checkTone,
  type GitConnectionCheck,
  type GitRepository,
  type ProviderCard,
  pickBranch,
} from '@/lib/integrations'
import { gitBranchesQuery, gitRepositoriesQuery } from '@/lib/integrations-api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

const markClasses: Record<ProviderCard, string> = {
  github: 'bg-foreground/10 text-foreground',
  gitlab: 'bg-orange-500/15 text-orange-600 dark:text-orange-400',
  gitea: 'bg-green-600/15 text-green-700 dark:text-green-400',
  forgejo: 'bg-amber-500/15 text-amber-700 dark:text-amber-400',
}

/** A provider's mark: a tinted tile (the name is always written next to it). */
export function ProviderMark({ card, className }: { card: ProviderCard; className?: string }) {
  return (
    <span
      aria-hidden="true"
      className={cn(
        'inline-grid size-8 shrink-0 place-items-center rounded-md',
        markClasses[card],
        className,
      )}
    >
      <GitBranchIcon className="size-4" />
    </span>
  )
}

/** What a token check found: the account, its scopes and what it lacks, or why it failed. */
export function CheckResult({ check }: { check: GitConnectionCheck }) {
  const { t } = usePrefs()
  const tone = checkTone(check)
  return (
    <div role="status" className="space-y-2 rounded-lg border p-3 text-sm">
      <p className="flex flex-wrap items-center gap-2">
        <ToneBadge tone={tone}>
          {check.ok ? (tone === 'warning' ? t('git.checkPartly') : t('git.checkOk')) : t('git.checkFailed')}
        </ToneBadge>
        {check.username && (
          <span>
            {t('git.signedInAs')}{' '}
            <span dir="ltr" className="font-medium font-mono">
              {check.username}
            </span>
          </span>
        )}
      </p>
      {check.error && (
        <p dir="auto" className="text-destructive">
          {check.error}
        </p>
      )}
      {check.scopes.length > 0 && (
        <p className="flex flex-wrap items-center gap-1">
          <span className="text-muted-foreground">{t('git.scopes')}</span>
          {check.scopes.map((s) => (
            <Tag key={s}>{s}</Tag>
          ))}
        </p>
      )}
      {check.missingScopes.length > 0 && (
        <p className="flex flex-wrap items-center gap-1">
          <span className="text-warning">{t('git.missingScopes')}</span>
          {check.missingScopes.map((s) => (
            <Tag key={s} className="border-warning/30 bg-warning/10 text-warning">
              {s}
            </Tag>
          ))}
        </p>
      )}
    </div>
  )
}

/** `value` after the reader stopped typing for a moment. */
function useSettled(value: string, ms = 300): string {
  const [settled, setSettled] = useState(value)
  useEffect(() => {
    const timer = setTimeout(() => setSettled(value), ms)
    return () => clearTimeout(timer)
  }, [value, ms])
  return settled
}

/**
 * The repositories a connection can read: a search box, one page at a time,
 * each a radio button; choosing one calls `onChange`.
 */
export function RepositoryPicker({
  connection,
  value,
  onChange,
}: {
  connection: string
  value: string
  onChange: (repository: GitRepository) => void
}) {
  const { t, locale } = usePrefs()
  const [search, setSearch] = useState('')
  const settled = useSettled(search.trim())
  // The page of this search; a new search starts on its first page.
  const [paging, setPaging] = useState({ search: '', page: 1 })
  const page = paging.search === settled ? paging.page : 1
  const setPage = (next: number) => setPaging({ search: settled, page: Math.max(1, next) })
  const searchId = useId()
  const name = useId()
  const list = useQuery(gitRepositoriesQuery(connection, settled, page))
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium' })
  const repositories = list.data?.repositories ?? []

  return (
    <div className="grid gap-3">
      <div className="grid gap-2">
        <label htmlFor={searchId} className="font-medium text-sm">
          {t('git.searchRepositories')}
        </label>
        <Input
          id={searchId}
          type="search"
          dir="ltr"
          value={search}
          placeholder="acme/shop"
          autoComplete="off"
          onChange={(e) => setSearch(e.target.value)}
        />
      </div>
      {list.isError ? (
        <ErrorAlert error={list.error} />
      ) : list.isPending ? (
        <Loading lines={3} />
      ) : repositories.length === 0 ? (
        <p className="text-muted-foreground text-sm">{t('git.noRepositories')}</p>
      ) : (
        <fieldset className="grid max-h-72 gap-1 overflow-y-auto rounded-lg border p-1">
          <legend className="sr-only">{t('git.repositories')}</legend>
          {repositories.map((r) => (
            <label
              key={r.fullName}
              className="flex cursor-pointer items-start gap-3 rounded-md px-2 py-2 hover:bg-accent/60 has-[:checked]:bg-accent has-[:focus-visible]:ring-[3px] has-[:focus-visible]:ring-ring/50"
            >
              <input
                type="radio"
                name={name}
                value={r.fullName}
                checked={value === r.fullName}
                onChange={() => onChange(r)}
                className="mt-1 accent-primary"
              />
              <span className="min-w-0 flex-1">
                <span className="flex flex-wrap items-center gap-2">
                  <span dir="ltr" className="break-all font-medium font-mono text-sm">
                    {r.fullName}
                  </span>
                  {r.private && (
                    <span className="inline-flex items-center gap-1 text-muted-foreground text-xs">
                      <LockIcon aria-hidden="true" className="size-3" />
                      {t('git.private')}
                    </span>
                  )}
                </span>
                {r.description && (
                  <span dir="auto" className="line-clamp-1 block text-muted-foreground text-xs">
                    {r.description}
                  </span>
                )}
                <span className="block text-muted-foreground text-xs">
                  {r.defaultBranch && (
                    <span dir="ltr" className="font-mono">
                      {r.defaultBranch}
                    </span>
                  )}
                  {r.updatedAt != null && (
                    <>
                      {r.defaultBranch && ' · '}
                      <time dateTime={new Date(r.updatedAt).toISOString()}>{date.format(r.updatedAt)}</time>
                    </>
                  )}
                </span>
              </span>
            </label>
          ))}
        </fieldset>
      )}
      {(page > 1 || list.data?.hasMore) && (
        <nav aria-label={t('git.repositoryPages')} className="flex items-center justify-between gap-2">
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={page <= 1 || list.isFetching}
            onClick={() => setPage(page - 1)}
          >
            <ChevronLeftIcon aria-hidden="true" className="rtl:rotate-180" />
            {t('table.previous')}
          </Button>
          <span className="text-muted-foreground text-xs" aria-live="polite">
            {fill(t('git.page'), { page })}
          </span>
          <Button
            type="button"
            variant="outline"
            size="sm"
            disabled={!list.data?.hasMore || list.isFetching}
            onClick={() => setPage(page + 1)}
          >
            {t('table.next')}
            <ChevronRightIcon aria-hidden="true" className="rtl:rotate-180" />
          </Button>
        </nav>
      )}
    </div>
  )
}

/**
 * The branches of one repository as a select; the first time they load, the
 * default branch (or `fallback`) is chosen unless `value` names one.
 */
export function BranchPicker({
  connection,
  repository,
  value,
  fallback,
  onChange,
}: {
  connection: string
  repository: string
  value: string
  fallback?: string | null
  onChange: (branch: string) => void
}) {
  const { t } = usePrefs()
  const branches = useQuery(gitBranchesQuery(connection, repository))
  const list = branches.data
  useEffect(() => {
    if (!list) return
    const pick = pickBranch(list, value, fallback)
    if (pick !== value) onChange(pick)
  }, [list, value, fallback, onChange])

  if (branches.isError) return <ErrorAlert error={branches.error} />
  return (
    <SelectInput
      label={t('git.branch')}
      value={value}
      disabled={!list || list.length === 0}
      dir="ltr"
      onChange={(e) => onChange(e.target.value)}
      hint={list && list.length === 0 ? t('git.noBranches') : undefined}
    >
      {!list ? (
        <option value={value}>{t('common.loading')}</option>
      ) : (
        list.map((b) => (
          <option key={b.name} value={b.name}>
            {b.name}
            {b.default ? ` (${t('git.defaultBranchShort')})` : ''}
            {b.protected ? ` · ${t('git.protected')}` : ''}
          </option>
        ))
      )}
    </SelectInput>
  )
}
