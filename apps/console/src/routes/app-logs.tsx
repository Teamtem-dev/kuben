import { useQuery } from '@tanstack/react-query'
import { EraserIcon, PauseIcon, PlayIcon } from 'lucide-react'
import { useEffect, useId, useRef, useState } from 'react'
import { ErrorAlert, Section } from '@/components/kit'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { NativeSelect, NativeSelectOption } from '@/components/ui/native-select'
import { ToggleGroup, ToggleGroupItem } from '@/components/ui/toggle-group'
import { followLogsUrl, type LogOptions, logsQuery } from '@/lib/api'
import { appendLines, endFrom, lineFrom, type ShownLine, splitTimestamp } from '@/lib/logStream'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

type Where = { project: string; environment: string; app: string }
type Mode = 'follow' | 'recent' | 'previous'

/** A followed log: lines as they come, bounded, with pause. */
function useFollow(url: string, enabled: boolean, paused: boolean) {
  const [lines, setLines] = useState<ShownLine[]>([])
  const [state, setState] = useState<'live' | 'reconnecting' | 'ended'>('live')
  const pausedRef = useRef(paused)
  pausedRef.current = paused

  useEffect(() => {
    if (!enabled) return
    setLines([])
    setState('live')
    let id = 0
    let batch: ShownLine[] = []
    let frame = 0
    // Many lines a second render once a frame.
    const flush = () => {
      frame = 0
      const more = batch
      batch = []
      setLines((current) => appendLines(current, more))
    }
    const push = (line: ShownLine) => {
      if (pausedRef.current && !line.note) return
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
      if (!end) return
      push(end.note)
      if (end.final) {
        source.close()
        setState('ended')
      }
    })
    return () => {
      source.close()
      if (frame) cancelAnimationFrame(frame)
    }
  }, [url, enabled])

  return { lines, state, clear: () => setLines([]) }
}

function Lines({ lines, emptyText }: { lines: readonly ShownLine[]; emptyText: string }) {
  const box = useRef<HTMLPreElement>(null)
  const pinned = useRef(true)
  // Stay at the bottom unless the reader scrolled up.
  useEffect(() => {
    const el = box.current
    if (el && pinned.current) el.scrollTop = el.scrollHeight
  })
  const pods = new Set(lines.filter((l) => !l.note).map((l) => l.pod))
  return (
    <pre
      ref={box}
      dir="ltr"
      // biome-ignore lint/a11y/noNoninteractiveTabindex: a scrolling region must be reachable from the keyboard (WCAG 2.1.1)
      tabIndex={0}
      onScroll={(e) => {
        const el = e.currentTarget
        pinned.current = el.scrollHeight - el.scrollTop - el.clientHeight < 24
      }}
      className="h-[28rem] overflow-auto rounded-lg border bg-muted/50 p-3 text-start font-mono text-xs leading-relaxed"
    >
      {lines.length === 0
        ? emptyText
        : lines.map((l) => {
            const { time, text } = l.time ? { time: l.time, text: l.text } : splitTimestamp(l.text)
            return (
              <div key={l.id} className={cn('whitespace-pre-wrap break-all', l.note && 'text-warning')}>
                {pods.size > 1 && <span className="text-muted-foreground">[{l.pod}] </span>}
                {time && <span className="text-muted-foreground">{time.slice(11, 19)} </span>}
                {text}
              </div>
            )
          })}
    </pre>
  )
}

export function LiveLogs({ project, environment, app, processes }: Where & { processes: readonly string[] }) {
  const { t } = usePrefs()
  const [tail, setTail] = useState(200)
  const [process, setProcess] = useState('')
  const [mode, setMode] = useState<Mode>('follow')
  const [paused, setPaused] = useState(false)
  const tailId = useId()
  const processId = useId()
  const options: LogOptions = { tail, previous: mode === 'previous', process: process || undefined }
  const follow = useFollow(followLogsUrl(project, environment, app, options), mode === 'follow', paused)
  const once = useQuery({
    ...logsQuery(project, environment, app, options),
    enabled: mode !== 'follow',
    retry: false,
  })
  const onceLines: ShownLine[] =
    once.data?.flatMap((pod, p) =>
      pod.error
        ? [{ id: p * 1_000_000, pod: pod.pod, text: pod.error, note: true }]
        : pod.lines.map((text, i) => ({ id: p * 1_000_000 + i + 1, pod: pod.pod, text })),
    ) ?? []

  const status =
    mode !== 'follow'
      ? null
      : paused
        ? t('logs.paused')
        : t(`logs.${follow.state === 'live' ? 'live' : follow.state}`)
  const live = mode === 'follow' && !paused && follow.state === 'live'

  return (
    <Section
      title={
        <span className="flex items-center gap-2">
          {t('logs.title')}
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
    >
      <div className="flex flex-wrap items-center gap-2">
        <ToggleGroup
          type="single"
          variant="outline"
          size="sm"
          value={mode}
          onValueChange={(value) => value && setMode(value as Mode)}
          aria-label={t('logs.mode')}
        >
          {(['follow', 'recent', 'previous'] as const).map((m) => (
            <ToggleGroupItem key={m} value={m} className="px-3">
              {m === 'follow' ? t('logs.follow') : m === 'previous' ? t('logs.previous') : t('logs.lines')}
            </ToggleGroupItem>
          ))}
        </ToggleGroup>
        {processes.length > 1 && (
          <>
            <label htmlFor={processId} className="sr-only">
              {t('logs.process')}
            </label>
            <NativeSelect
              id={processId}
              size="sm"
              value={process}
              onChange={(e) => setProcess(e.target.value)}
            >
              <NativeSelectOption value="">{t('logs.allProcesses')}</NativeSelectOption>
              {processes.map((p) => (
                <NativeSelectOption key={p} value={p}>
                  {p}
                </NativeSelectOption>
              ))}
            </NativeSelect>
          </>
        )}
        <label htmlFor={tailId} className="sr-only">
          {t('logs.lines')}
        </label>
        <NativeSelect id={tailId} size="sm" value={tail} onChange={(e) => setTail(Number(e.target.value))}>
          {[50, 200, 500, 1000].map((n) => (
            <NativeSelectOption key={n} value={n}>
              {n}
            </NativeSelectOption>
          ))}
        </NativeSelect>
        {mode === 'follow' && (
          <div className="ms-auto flex gap-1">
            <Button variant="ghost" size="sm" onClick={() => setPaused((p) => !p)} aria-pressed={paused}>
              {paused ? <PlayIcon aria-hidden="true" /> : <PauseIcon aria-hidden="true" />}
              {paused ? t('logs.resume') : t('logs.pause')}
            </Button>
            <Button variant="ghost" size="sm" onClick={follow.clear}>
              <EraserIcon aria-hidden="true" />
              {t('logs.clear')}
            </Button>
          </div>
        )}
      </div>
      {mode === 'follow' ? (
        <Lines lines={follow.lines} emptyText={t('logs.empty')} />
      ) : once.isError ? (
        <ErrorAlert error={once.error} />
      ) : once.data && once.data.length === 0 ? (
        <p className="text-muted-foreground text-sm">{t('logs.noPods')}</p>
      ) : (
        <Lines lines={onceLines} emptyText={once.isLoading ? t('common.loading') : t('logs.empty')} />
      )}
    </Section>
  )
}
