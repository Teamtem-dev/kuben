import { describe, expect, it } from 'bun:test'
import { ApiError, isProblem, problemMessage, toApiError } from './problem'

const validation = {
  code: 'validation',
  title: 'Unprocessable Entity',
  status: 422,
  detail: 'name must be a DNS-1123 label',
}

describe('isProblem', () => {
  it('recognises problem bodies only', () => {
    expect(isProblem(validation)).toBe(true)
    expect(isProblem({ message: 'nope' })).toBe(false)
    expect(isProblem(null)).toBe(false)
  })
})

describe('toApiError', () => {
  it('keeps status, code and detail', () => {
    const err = toApiError(validation, 422)
    expect(err).toBeInstanceOf(ApiError)
    expect([err.status, err.code, err.message]).toEqual([422, 'validation', validation.detail])
  })

  it('wraps bodies that are not problems', () => {
    const err = toApiError('<html>bad gateway</html>', 502)
    expect([err.status, err.code]).toEqual([502, 'unknown'])
  })
})

describe('problemMessage', () => {
  it('prefers detail, then title', () => {
    expect(problemMessage(validation)).toBe(validation.detail)
    expect(problemMessage({ code: 'forbidden', title: 'Forbidden', status: 403 })).toBe('Forbidden')
  })

  it('uses the message of thrown API errors', () => {
    expect(problemMessage(toApiError(validation, 422))).toBe(validation.detail)
  })

  it('falls back for unknown values', () => {
    expect(problemMessage(new Error('offline'))).toBe('offline')
    expect(problemMessage(42)).toBe('Something went wrong.')
  })
})
