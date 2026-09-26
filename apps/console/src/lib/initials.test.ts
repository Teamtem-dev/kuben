import { expect, test } from 'bun:test'
import { initials } from './initials'

test('initials come from the first two words of a name or an email', () => {
  expect(initials('Ada Lovelace')).toBe('AL')
  expect(initials('owner@example.com')).toBe('OE')
  expect(initials('jane.doe-smith')).toBe('JD')
  expect(initials('x')).toBe('X')
  expect(initials('')).toBe('')
})
