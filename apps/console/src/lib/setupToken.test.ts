import { describe, expect, test } from 'bun:test'
import { setupTokenFrom, tunnelFor } from './setupToken'

describe('setupTokenFrom', () => {
  test('reads the fragment first', () => {
    expect(setupTokenFrom('#token=abc', 'old')).toBe('abc')
    expect(setupTokenFrom('#token=%20abc%20')).toBe('abc')
  })
  test('falls back to the query of older links', () => {
    expect(setupTokenFrom('', 'old')).toBe('old')
    expect(setupTokenFrom('#other=1', ' old ')).toBe('old')
  })
  test('is empty without a token', () => {
    expect(setupTokenFrom('', undefined)).toBeUndefined()
    expect(setupTokenFrom('#token=', '')).toBeUndefined()
  })
})

describe('tunnelFor', () => {
  test('forwards the console port and keeps the token in the fragment', () => {
    expect(tunnelFor('203.0.113.7', '3000', 'abc')).toEqual({
      command: 'ssh -L 3000:127.0.0.1:3000 <you>@203.0.113.7',
      link: 'http://localhost:3000/setup#token=abc',
    })
    expect(tunnelFor('kuben.example.com', '').link).toBe('http://localhost:80/setup')
  })
})
