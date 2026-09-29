/**
 * The builds tab's live parts: "Build now", a build's stage timeline and its
 * log (followed while the build runs, the kept tail after it settled).
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { useNavigate } from '@tanstack/react-router'
import { DownloadIcon, HammerIcon } from 'lucide-react'
import { useEffect, useRef, useState } from 'react'
import { CopyButton, ErrorAlert, Notice, Section, SwitchField, ToneBadge } from '@/components/kit'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  type Build,
  buildLogsUrl,
  elapsed,
  isFinalBuild,
  logLines,
  stageElapsed,
  stagesOf,
  stageTone,
} from '@/lib/delivery'
import { buildLogQuery, triggerBuild } from '@/lib/delivery-api'
import { appendLines, endFrom, lineFrom, type ShownLine } from '@/lib/logStream'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

type Where = { project: string; environment: string; app: string }

/** Build the branch head now; opens the build once the API names it. */
export function BuildNow({ project, environment, app }: Where) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const navigate = useNavigate()
  const trigger = useMutation({
    mutationFn: () => triggerBuild(project, environment, app),
    onSuccess: async (result) => {
      await queryClient.invalidateQueries({ queryKey: ['app', project, environment, app, 'builds'] })
      if (result.build) {
        await navigate({
          to: '/projects/$project/$environment/$app',
          params: { project, environment, app },
          search: { tab: 'builds', build: result.build.id },
        })
      }
    },
  })
  return (
    <div className="flex flex-col items-end gap-2">
      <Button size="sm" disabled={trigger.isPending} onClick={() => trigger.mutate()}>
        <HammerIcon aria-hidden="true" />
        {trigger.isPending ? t('builds.starting') : t('builds.buildNow')}
      </Button>
      {trigger.data && !trigger.data.build && (
        <Notice tone="success" className="max-w-sm">
          {t('builds.syncQueued')}
        </Notice>
      )}
      <ErrorAlert error={trigger.error} className="max-w-sm" />
    </div>
  )
}

/** The stages in order: each one's state, how long it took and what it said. */
export function StageTimeline({ build, now }: { build: Build; now: number }) {
  const { t, tOr } = usePrefs()
  const stages = stagesOf(build)
  if (stages.length === 0) return null
  return (
    <ol aria-label={t('builds.stages')} className="grid gap-2 sm:grid-cols-3 lg:grid-cols-6">
      {stages.map((s) => {
        const ms = stageElapsed(s, now)
        return (
          <li
            key={s.name}
            aria-current={s.status === 'running' ? 'step' : undefined}
            className={cn(
              'space-y-1 rounded-lg border p-2.5 text-xs',
              s.status === 'running' && 'border-warning/50',
              s.status === 'failed' && 'border-destructive/50',
            )}
          >
            <p className="font-medium text-sm">{tOr(`buildStage.${s.name}`, s.name)}</p>
            <ToneBadge tone={stageTone(s.status)}>{tOr(`stageStatus.${s.status}`, s.status)}</ToneBadge>
            {ms != null && <p className="text-muted-foreground">{elapsed(ms)}</p>}
            {s.detail && (
              <p dir="auto" className="line-clamp-3 text-muted-foreground">
                {s.detail}
              </p>
            )}
          </li>
        )
      })}
    </ol>
  )
}

type FollowState = 'connecting' | 'live' | 'reconnecting' | 'ended'

/** The build's log as it is written: `line` events, until an `end`. */
function useFollowedLog(url: string, enabled: boolean) {
  const [lines, setLines] = useState<ShownLine[]>([])
  const [state, setState] = useState<FollowState>('connecting')
  useEffect(() => {
    if (!enabled) return
    setLines([])
    setState('connecting')
    let id = 0
    let batch: ShownLine[] = []
    let frame = 0
    const flush = () => {
      frame = 0
      const more = batch
      batch = []
      setLines((current) => appendLines(current, more))
    }
    const push = (line: ShownLine) => {
      batch.push(line)
      if (!frame) frame = requestAnimationFrame(flush)
    }
    const source = new EventSource(url)
    source.addEventListener('open', () => setState('live'))
    source.addEventListener('error', () =>
      setState(source.readyState === EventSource.CLOSED ? 'ended' : 'reconnecting'),
    )
    source.addEventListener('line', (e) => {
      const line = lineFrom((e as MessageEvent<string>).data, id++)
      if (line) push(line)
    })
    source.addEventListener('end', (e) => {
      const end = endFrom((e as MessageEvent<string>).data, id++)
      if (end?.note.text && end.note.text !== 'stream ended' && end.note.text !== 'log ended') push(end.note)
      source.close()
      setState('ended')
    })
    return () => {
      source.close()
      if (frame) cancelAnimationFrame(frame)
    }
  }, [url, enabled])
  return { lines, state }
}

/** The log of one build, with following, wrapping, copy and download. */
export function BuildLog({ project, environment, app, build }: Where & { build: Build }) {
  const { t } = usePrefs()
  const running = !isFinalBuild(build.phase)
  const follow = useFollowedLog(buildLogsUrl(project, environment, app, build.id, true), running)
  // Once the build settled, or when following could not start, read the kept log.
  const fallback = running && follow.state === 'ended' && follow.lines.length === 0
  const text = useQuery({
    ...buildLogQuery(project, environment, app, build.id),
    enabled: !running || fallback,
  })
  const [autoScroll, setAutoScroll] = useState(true)
  const [wrap, setWrap] = useState(true)
  const box = useRef<HTMLPreElement>(null)

  const lines: ShownLine[] =
    running && !fallback
      ? follow.lines
      : logLines(text.data ?? '').map((line, i) => ({ id: i, pod: '', text: line }))
  const all = lines.map((l) => l.text).join('\n')

  useEffect(() => {
    const el = box.current
    if (el && autoScroll && lines.length > 0) el.scrollTop = el.scrollHeight
  })

  const status = running && !fallback ? t(`builds.log.${follow.state}`) : null
  const live = status != null && follow.state === 'live'

  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          {t('builds.log')}
          {status && (
            <Badge role="status" variant="outline" className="font-normal text-muted-foreground">
              <span
                aria-hidden="true"
                className={cn(
                  'size-1.5 rounded-full',
                  live ? 'animate-pulse bg-success' : 'bg-muted-foreground',
                )}
              />
              {status}
            </Badge>
          )}
        </span>
      }
      actions={
        <>
          <CopyButton value={all} />
          <Button variant="outline" size="sm" asChild>
            <a href={buildLogsUrl(project, environment, app, build.id)} download={`build-${build.id}.log`}>
              <DownloadIcon aria-hidden="true" />
              {t('builds.downloadLog')}
            </a>
          </Button>
        </>
      }
    >
      <div className="flex flex-wrap items-center gap-4">
        <SwitchField label={t('builds.autoScroll')} checked={autoScroll} onCheckedChange={setAutoScroll} />
        <SwitchField label={t('builds.wrap')} checked={wrap} onCheckedChange={setWrap} />
        <span className="ms-auto text-muted-foreground text-xs">
          {fill(t('builds.logLines'), { count: lines.length })}
        </span>
      </div>
      {text.isError && (!running || fallback) ? (
        <ErrorAlert error={text.error} />
      ) : (
        <pre
          ref={box}
          dir="ltr"
          // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region must be reachable from the keyboard (WCAG 2.1.1)
          tabIndex={0}
          onScroll={(e) => {
            // Scrolling up to read stops following; back at the bottom, it follows again.
            const el = e.currentTarget
            const atBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 24
            if (atBottom !== autoScroll) setAutoScroll(atBottom)
          }}
          className="h-[28rem] overflow-auto rounded-lg border bg-muted/50 p-3 text-start font-mono text-xs leading-relaxed"
        >
          {lines.length === 0
            ? text.isFetching || (running && follow.state === 'connecting')
              ? t('common.loading')
              : t('builds.logEmpty')
            : lines.map((l) => (
                <div
                  key={l.id}
                  className={cn(
                    wrap ? 'whitespace-pre-wrap break-all' : 'whitespace-pre',
                    l.note && 'text-warning',
                  )}
                >
                  {l.text}
                </div>
              ))}
        </pre>
      )}
    </Section>
  )
}
