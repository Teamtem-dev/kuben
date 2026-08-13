import { useInfiniteQuery } from '@tanstack/react-query'
import { Button, Card, Empty, ErrorNote, PageHeader } from '../components/ui'
import { type AuditEvent, auditPage } from '../lib/api'

const outcomeStyle: Record<string, string> = {
  success: 'text-emerald-300',
  denied: 'text-red-300',
  throttled: 'text-amber-300',
  failure: 'text-amber-300',
  error: 'text-red-300',
}

function Row({ e }: { e: AuditEvent }) {
  return (
    <tr>
      <td className="whitespace-nowrap py-2 pe-4 text-slate-400">{new Date(e.at).toLocaleString()}</td>
      <td className="py-2 pe-4">{e.actor ?? e.actor_kind}</td>
      <td className="py-2 pe-4 font-mono text-xs">{e.action}</td>
      <td className="py-2 pe-4 font-mono text-slate-400 text-xs">{e.target ?? '—'}</td>
      <td className={`py-2 pe-4 ${outcomeStyle[e.outcome] ?? ''}`}>
        {e.outcome}
        {e.status ? ` (${e.status})` : ''}
      </td>
      <td className="py-2 font-mono text-slate-500 text-xs">{e.ip ?? '—'}</td>
    </tr>
  )
}

export function AuditPage() {
  const log = useInfiniteQuery({
