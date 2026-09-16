import { useQuery } from '@tanstack/react-query'
import { useEffect, useId, useRef, useState } from 'react'
import { Button, Card, ErrorNote } from '../components/ui'
import { followLogsUrl, type LogOptions, logsQuery } from '../lib/api'
import { appendLines, endFrom, lineFrom, type ShownLine, splitTimestamp } from '../lib/logStream'
import { usePrefs } from '../lib/prefs'

type Where = { project: string; environment: string; app: string }

const select = 'rounded-md border border-line bg-canvas px-2 py-1 text-xs'

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
      className="h-96 overflow-auto rounded-lg bg-inset p-3 font-mono text-fg-soft text-xs leading-relaxed"
    >
      {lines.length === 0
        ? emptyText
        : lines.map((l) => {
            const { time, text } = l.time ? { time: l.time, text: l.text } : splitTimestamp(l.text)
            return (
              <div key={l.id} className={l.note ? 'text-warn' : undefined}>
                {pods.size > 1 && <span className="text-subtle">[{l.pod}] </span>}
                {time && <span className="text-subtle">{time.slice(11, 19)} </span>}
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
  const [mode, setMode] = useState<'follow' | 'recent' | 'previous'>('follow')
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

  return (
    <Card
      title={
        <span className="flex items-center gap-2">
          {t('logs.title')}
          {status && (
            <span role="status" className="rounded bg-hover px-1.5 py-0.5 font-normal text-muted text-xs">
              {status}
            </span>
          )}
        </span>
      }
      actions={
        <div className="flex flex-wrap items-center gap-2 text-muted text-xs">
          <fieldset className="flex overflow-hidden rounded-md border border-line">
            <legend className="sr-only">{t('logs.title')}</legend>
            {(['follow', 'recent', 'previous'] as const).map((m) => (
              <label key={m} className={`cursor-pointer px-2 py-1 ${mode === m ? 'bg-hover text-fg' : ''}`}>
                <input
                  type="radio"
                  name={`${app}-log-mode`}
                  value={m}
                  checked={mode === m}
                  onChange={() => setMode(m)}
                  className="sr-only"
                />
                {m === 'follow' ? t('logs.follow') : m === 'previous' ? t('logs.previous') : t('logs.lines')}
              </label>
            ))}
          </fieldset>
          {processes.length > 1 && (
            <>
              <label htmlFor={processId} className="sr-only">
                {t('logs.process')}
              </label>
              <select
                id={processId}
                value={process}
                onChange={(e) => setProcess(e.target.value)}
                className={select}
              >
                <option value="">{t('logs.allProcesses')}</option>
                {processes.map((p) => (
                  <option key={p} value={p}>
                    {p}
                  </option>
                ))}
              </select>
            </>
          )}
          <label htmlFor={tailId} className="sr-only">
            {t('logs.lines')}
          </label>
          <select
            id={tailId}
            value={tail}
            onChange={(e) => setTail(Number(e.target.value))}
            className={select}
          >
            {[50, 200, 500, 1000].map((n) => (
              <option key={n} value={n}>
                {n}
              </option>
            ))}
          </select>
          {mode === 'follow' && (
            <>
              <Button variant="ghost" onClick={() => setPaused((p) => !p)} aria-pressed={paused}>
                {paused ? t('logs.resume') : t('logs.pause')}
              </Button>
              <Button variant="ghost" onClick={follow.clear}>
                {t('logs.clear')}
              </Button>
            </>
          )}
        </div>
      }
    >
      {mode === 'follow' ? (
        <Lines lines={follow.lines} emptyText={t('logs.empty')} />
      ) : once.isError ? (
        <ErrorNote error={once.error} />
      ) : once.data && once.data.length === 0 ? (
        <p className="text-subtle text-sm">{t('logs.noPods')}</p>
      ) : (
        <Lines lines={onceLines} emptyText={once.isLoading ? t('common.loading') : t('logs.empty')} />
      )}
    </Card>
  )
}
