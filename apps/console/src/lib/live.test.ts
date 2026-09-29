import { describe, expect, it, test } from 'bun:test'
import type { Build } from './delivery'
import { buildDelta, keysFor, upsertBuild } from './live'

describe('keysFor', () => {
  it('maps stream deltas to the queries they affect', () => {
    expect(keysFor('pod_upsert')).toEqual(['app'])
    expect(keysFor('exposure_changed')).toEqual(['apps', 'app'])
    expect(keysFor('project_delete')).toEqual(['projects'])
    expect(keysFor('environment_upsert')).toEqual(['environments', 'projects'])
    expect(keysFor('app_upsert')).toEqual(['apps', 'app'])
    expect(keysFor('build')).toEqual(['app'])
    expect(keysFor('build_upsert')).toEqual(['app'])
    expect(keysFor('builder')).toEqual([])
    expect(keysFor('something_else')).toEqual([])
  })
})

const build = (id: string, createdAt: number, phase = 'running') => ({ id, createdAt, phase }) as Build

describe('build deltas', () => {
  const delta = {
    kind: 'build',
    seq: 7,
    org: 'o1',
    app: 'kb-shop-prod/web',
    project: 'shop',
    environment: 'prod',
    name: 'web',
    build: build('b2', 2),
  }

  test('a whole build delta names the app and carries the build', () => {
    expect(buildDelta(delta)).toEqual({
      project: 'shop',
      environment: 'prod',
      app: 'web',
      build: build('b2', 2),
    })
  })

  test('anything else is not one', () => {
    expect(buildDelta(null)).toBeNull()
    expect(buildDelta({ ...delta, kind: 'pod_upsert' })).toBeNull()
    expect(buildDelta({ ...delta, name: undefined })).toBeNull()
    expect(buildDelta({ ...delta, build: null })).toBeNull()
    expect(buildDelta({ ...delta, build: { id: 'b2' } })).toBeNull()
  })

  test('a build replaces its old self, or joins the list newest first', () => {
    const list = [build('b1', 1, 'succeeded')]
    expect(upsertBuild(list, build('b1', 1, 'failed'))).toEqual([build('b1', 1, 'failed')])
    expect(upsertBuild(list, build('b2', 2)).map((b) => b.id)).toEqual(['b2', 'b1'])
    expect(upsertBuild([build('b3', 3)], build('b0', 0)).map((b) => b.id)).toEqual(['b3', 'b0'])
  })
})
