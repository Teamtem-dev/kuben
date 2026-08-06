import { describe, expect, it } from 'vitest'
import { formatEnvLines, parseEnvLines } from './env'

describe('parseEnvLines', () => {
  it('parses values, secret references, comments and blanks', () => {
    const { vars, errors } = parseEnvLines(
      '# comment\nLOG_LEVEL=info\n\nDATABASE_URL=@db/url\nEMPTY=\nURL=a=b',
    )
    expect(errors).toEqual([])
    expect(vars).toEqual([
      { name: 'LOG_LEVEL', value: 'info' },
      { name: 'DATABASE_URL', secret: { name: 'db', key: 'url' } },
      { name: 'EMPTY', value: '' },
      { name: 'URL', value: 'a=b' },
    ])
  })

  it('reports invalid names and duplicates with line numbers', () => {
    const { vars, errors } = parseEnvLines('1BAD=x\nOK=1\nOK=2')
    expect(vars).toEqual([{ name: 'OK', value: '1' }])
    expect(errors).toEqual(['line 1: "1BAD" is not a valid variable name', 'line 3: OK is set twice'])
  })

  it('round-trips through formatEnvLines', () => {
    const text = 'A=1\nB=@creds/token'
    expect(formatEnvLines(parseEnvLines(text).vars)).toBe(text)
  })
})
