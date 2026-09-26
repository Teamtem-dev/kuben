import { expect, test } from 'bun:test'
import { appTabFrom } from './app-tabs'

test('the tab comes from ?tab= when known; the default and anything else is left out', () => {
  expect(appTabFrom('logs')).toBe('logs')
  expect(appTabFrom('deployments')).toBe('deployments')
  expect(appTabFrom('releases')).toBe('releases')
  expect(appTabFrom('settings')).toBe('settings')
  expect(appTabFrom('overview')).toBeUndefined()
  expect(appTabFrom('nope')).toBeUndefined()
  expect(appTabFrom(3)).toBeUndefined()
  expect(appTabFrom(undefined)).toBeUndefined()
})
