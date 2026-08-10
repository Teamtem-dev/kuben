import { useInfiniteQuery } from '@tanstack/react-query'
import { Button, Card, Empty, ErrorNote, PageHeader } from '../components/ui'
import { type AuditEvent, auditPage } from '../lib/api'

const outcomeStyle: Record<string, string> = {
  success: 'text-emerald-300',
  denied: 'text-red-300',
  throttled: 'text-amber-300',
  failure: 'text-amber-300',
  error: 'text-red-300',
