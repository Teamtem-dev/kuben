import { describe, expect, test } from 'bun:test'
import {
  approvalsLeft,
  buildElapsed,
  buildLogsUrl,
  buildTone,
  COMMENT_MAX,
  canCancelBuild,
  decideBody,
  elapsed,
  imageDigest,
  isAwaitingApproval,
  isFinalBuild,
  logLines,
  sbomUrl,
  shortCommit,
  shortId,
  stageElapsed,
  stagesOf,
  stageTone,
} from './delivery'

describe('approvals', () => {
  test('a run waits for approval in the awaitingApproval phase only', () => {
    expect(isAwaitingApproval('awaitingApproval')).toBe(true)
    expect(isAwaitingApproval('planned')).toBe(false)
    expect(isAwaitingApproval('pendingDelivery')).toBe(false)
  })

  test('a decision sends back the plan hash, and the comment only when there is one', () => {
    expect(decideBody('ab12', '')).toEqual({ planHash: 'ab12' })
    expect(decideBody('ab12', '   ')).toEqual({ planHash: 'ab12' })
    expect(decideBody('ab12', '  looks right ')).toEqual({ planHash: 'ab12', comment: 'looks right' })
    expect(decideBody('ab12', 'x'.repeat(COMMENT_MAX + 5))?.comment).toHaveLength(COMMENT_MAX)
  })

  test('no plan hash, no decision', () => {
    expect(decideBody(null, 'ok')).toBeNull()
    expect(decideBody(undefined, '')).toBeNull()
    expect(decideBody('', '')).toBeNull()
  })

  test('approvals still needed never go below zero', () => {
    expect(approvalsLeft({ required: 2, approved: 0 })).toBe(2)
    expect(approvalsLeft({ required: 2, approved: 1 })).toBe(1)
    expect(approvalsLeft({ required: 1, approved: 3 })).toBe(0)
  })
})

describe('elapsed', () => {
  test('seconds, minutes and hours', () => {
    expect(elapsed(0)).toBe('0.0s')
    expect(elapsed(-50)).toBe('0.0s')
    expect(elapsed(12_400)).toBe('12s')
    expect(elapsed(9_960)).toBe('10.0s')
    expect(elapsed(59_900)).toBe('59s')
    expect(elapsed(185_000)).toBe('3m 05s')
    expect(elapsed(119_900)).toBe('1m 59s')
    expect(elapsed(3_720_000)).toBe('1h 02m')
  })
})

describe('builds', () => {
  test('succeeded, failed and cancelled are final', () => {
    for (const phase of ['succeeded', 'failed', 'cancelled']) expect(isFinalBuild(phase)).toBe(true)
    for (const phase of ['queued', 'blocked', 'running', 'publishing', 'verifyingOutput', 'cancelling'])
      expect(isFinalBuild(phase)).toBe(false)
  })

  test('a build can be cancelled while under way and nobody asked yet', () => {
    expect(canCancelBuild({ phase: 'running', cancelRequested: false })).toBe(true)
    expect(canCancelBuild({ phase: 'queued', cancelRequested: false })).toBe(true)
    expect(canCancelBuild({ phase: 'running', cancelRequested: true })).toBe(false)
    expect(canCancelBuild({ phase: 'cancelRequested', cancelRequested: false })).toBe(false)
    expect(canCancelBuild({ phase: 'cancelling', cancelRequested: false })).toBe(false)
    expect(canCancelBuild({ phase: 'succeeded', cancelRequested: false })).toBe(false)
    expect(canCancelBuild({ phase: 'failed', cancelRequested: false })).toBe(false)
  })

  test('the phase colour', () => {
    expect(buildTone('succeeded')).toBe('success')
    expect(buildTone('failed')).toBe('danger')
    expect(buildTone('blocked')).toBe('warning')
    expect(buildTone('cancelling')).toBe('warning')
    expect(buildTone('running')).toBe('neutral')
    expect(buildTone('cancelled')).toBe('neutral')
  })

  test('how long a build ran, or has been running', () => {
    expect(buildElapsed({ startedAt: null, finishedAt: null }, 5_000)).toBeNull()
    expect(buildElapsed({ startedAt: 1_000, finishedAt: 4_000 }, 99_000)).toBe(3_000)
    expect(buildElapsed({ startedAt: 1_000, finishedAt: null }, 6_000)).toBe(5_000)
    expect(buildElapsed({ startedAt: 6_000, finishedAt: null }, 1_000)).toBe(0)
  })

  test('short commits and ids', () => {
    expect(shortCommit('0123456789abcdef0123456789abcdef01234567')).toBe('0123456')
    expect(shortCommit('main')).toBe('main')
    expect(shortId('0190f3c6-0000-7000-8000-0000000000b1')).toBe('0190f3c6')
    expect(shortId('build-1')).toBe('build-1')
  })

  test('the digest of an image reference', () => {
    expect(imageDigest('ghcr.io/acme/web@sha256:abc')).toBe('sha256:abc')
    expect(imageDigest('ghcr.io/acme/web:1.2')).toBeNull()
    expect(imageDigest('ghcr.io/acme/web@')).toBeNull()
    expect(imageDigest(null)).toBeNull()
  })

  test('the SBOM address escapes every part', () => {
    expect(sbomUrl('shop', 'prod', 'web', 'sha256:abc')).toBe(
      '/api/v1/projects/shop/environments/prod/apps/web/sbom/sha256%3Aabc',
    )
  })
})

describe('build stages and logs', () => {
  const stage = (name: 'clone' | 'plan' | 'build' | 'scan' | 'push' | 'deploy') => ({
    name,
    status: 'succeeded' as const,
  })

  test('stages come in build order, whatever order the API sent', () => {
    expect(stagesOf({ stages: [stage('push'), stage('clone'), stage('build')] }).map((s) => s.name)).toEqual([
      'clone',
      'build',
      'push',
    ])
    expect(stagesOf({})).toEqual([])
    expect(stagesOf({ stages: undefined })).toEqual([])
  })

  test('the stage colour and how long it ran', () => {
    expect(stageTone('succeeded')).toBe('success')
    expect(stageTone('failed')).toBe('danger')
    expect(stageTone('running')).toBe('warning')
    expect(stageTone('pending')).toBe('neutral')
    expect(stageTone('skipped')).toBe('neutral')
    expect(stageElapsed({ startedAt: null }, 9)).toBeNull()
    expect(stageElapsed({ startedAt: 1_000, finishedAt: 3_000 }, 9_000)).toBe(2_000)
    expect(stageElapsed({ startedAt: 1_000, finishedAt: null }, 4_000)).toBe(3_000)
  })

  test('the log address escapes every part and follows on request', () => {
    expect(buildLogsUrl('shop', 'prod', 'web', 'a/b')).toBe(
      '/api/v1/projects/shop/environments/prod/apps/web/builds/a%2Fb/logs',
    )
    expect(buildLogsUrl('shop', 'prod', 'web', 'b1', true)).toBe(
      '/api/v1/projects/shop/environments/prod/apps/web/builds/b1/logs?follow=true',
    )
  })

  test('a log splits into lines without the empty last one', () => {
    expect(logLines('')).toEqual([])
    expect(logLines('a\nb\n')).toEqual(['a', 'b'])
    expect(logLines('a\r\n\nb')).toEqual(['a', '', 'b'])
  })
})
