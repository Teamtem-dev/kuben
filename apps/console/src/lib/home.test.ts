import { expect, test } from 'bun:test'
import type { AuditEvent } from './api'
import { recentDeployments, subsystemsOf } from './home'

const event = (seq: number, action: string, outcome = 'success'): AuditEvent => ({
  seq,
  id: `e${seq}`,
  at: seq * 1000,
  actor_kind: 'user',
  actor: 'owner@example.com',
  action,
  target_kind: 'app',
  target: 'shop/prod/web',
  outcome,
  status: 200,
  ip: null,
  request_id: null,
})

test('recent deployments: deploys, rollbacks and promotions that went through, newest first', () => {
  const events = [
    event(1, 'startDeployment', 'accepted'),
    event(2, 'updateApp'),
    event(3, 'rollbackApp'),
    event(4, 'promoteApp', 'denied'),
    event(5, 'emergencyRollback'),
    event(6, 'promoteApp'),
  ]
  expect(recentDeployments(events, 10).map((e) => e.seq)).toEqual([6, 5, 3, 1])
  expect(recentDeployments(events, 2).map((e) => e.seq)).toEqual([6, 5])
})

test('subsystems: sorted by name; odd shapes read as unknown', () => {
  expect(
    subsystemsOf({
      supervisor: { state: 'ok', updated_at_ms: 1 },
      agentlink: { state: 'degraded', last_error: 'no agent', updated_at_ms: 1 },
      odd: 'x',
    }),
  ).toEqual([
    { name: 'agentlink', state: 'degraded', lastError: 'no agent' },
    { name: 'odd', state: 'unknown', lastError: undefined },
    { name: 'supervisor', state: 'ok', lastError: undefined },
  ])
  expect(subsystemsOf(null)).toEqual([])
})
