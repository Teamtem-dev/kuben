import { expect, test } from 'bun:test'
import { appTabFrom, appTabs, buildFrom, shownAppTab } from './app-tabs'

test('the tab comes from ?tab= when known; the default and anything else is left out', () => {
  expect(appTabFrom('logs')).toBe('logs')
  expect(appTabFrom('deployments')).toBe('deployments')
  expect(appTabFrom('builds')).toBe('builds')
  expect(appTabFrom('releases')).toBe('releases')
  expect(appTabFrom('settings')).toBe('settings')
  expect(appTabFrom('overview')).toBeUndefined()
  expect(appTabFrom('nope')).toBeUndefined()
  expect(appTabFrom(3)).toBeUndefined()
  expect(appTabFrom(undefined)).toBeUndefined()
})

test('builds is a tab of Git-sourced apps only', () => {
  expect(appTabs(true)).toContain('builds')
  expect(appTabs(false)).not.toContain('builds')
  expect(appTabs(false)).toEqual(['overview', 'deployments', 'releases', 'logs', 'settings'])
})

test('a tab the app does not have shows the overview', () => {
  expect(shownAppTab('builds', true)).toBe('builds')
  expect(shownAppTab('builds', false)).toBe('overview')
  expect(shownAppTab('logs', false)).toBe('logs')
  expect(shownAppTab(undefined, true)).toBe('overview')
})

test('?build= is a non-empty string or nothing', () => {
  expect(buildFrom('b-1')).toBe('b-1')
  expect(buildFrom('')).toBeUndefined()
  expect(buildFrom(' ')).toBeUndefined()
  expect(buildFrom(7)).toBeUndefined()
})
