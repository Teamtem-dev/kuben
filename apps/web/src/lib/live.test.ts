import { describe, expect, it } from 'bun:test'
import { keysFor } from './live'

describe('keysFor', () => {
  it('maps stream deltas to the queries they affect', () => {
    expect(keysFor('pod_upsert')).toEqual(['app'])
    expect(keysFor('project_delete')).toEqual(['projects'])
    expect(keysFor('environment_upsert')).toEqual(['environments', 'projects'])
    expect(keysFor('app_upsert')).toEqual(['apps', 'app'])
    expect(keysFor('something_else')).toEqual([])
  })
})
