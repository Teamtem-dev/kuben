import { describe, expect, test } from 'bun:test'
import { crumbsFor } from './breadcrumbs'

describe('crumbsFor', () => {
  test('top-level pages', () => {
    expect(crumbsFor('/')).toEqual([{ kind: 'page', label: 'nav.projects' }])
    expect(crumbsFor('/team')).toEqual([{ kind: 'page', label: 'nav.team' }])
    expect(crumbsFor('/account')).toEqual([{ kind: 'page', label: 'shell.account' }])
    expect(crumbsFor('/nowhere')).toEqual([])
  })

  test('the project hierarchy, down to Doctor', () => {
    expect(crumbsFor('/projects/shop/prod/web/doctor')).toEqual([
      { kind: 'page', label: 'nav.projects', to: '/' },
      { kind: 'project', project: 'shop' },
      { kind: 'environment', project: 'shop', environment: 'prod' },
      { kind: 'app', project: 'shop', environment: 'prod', app: 'web' },
      { kind: 'page', label: 'doctor.title' },
    ])
    expect(crumbsFor('/projects/shop/')).toEqual([
      { kind: 'page', label: 'nav.projects', to: '/' },
      { kind: 'project', project: 'shop' },
    ])
  })
})
