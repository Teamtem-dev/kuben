import { expect, test } from 'bun:test'
import { evidenceNodes } from './evidence'

test('nodes come upstream first, each with the layers it depends on', () => {
  const nodes = evidenceNodes({
    nodes: [
      {
        layer: 'tls',
        status: 'warn',
        subject: 'web.example.com',
        evidence: ['no certificate'],
        action: 'set an issuer',
      },
      { layer: 'dns', status: 'fail', subject: 'web.example.com', evidence: ['NXDOMAIN'], observedAt: 1000 },
      { layer: 'gateway', status: 'ok', subject: 'kuben', evidence: [], action: null },
    ],
    edges: [
      { from: 'gateway', to: 'tls' },
      { from: 'dns', to: 'tls' },
    ],
  })
  expect(nodes.map((n) => n.layer)).toEqual(['gateway', 'dns', 'tls'])
  expect(nodes[2]?.causes).toEqual(['gateway', 'dns'])
  expect(nodes[2]?.action).toBe('set an issuer')
  expect(nodes[1]?.observedAt).toBe(1000)
  expect(nodes[0]?.causes).toEqual([])
  expect(nodes[0]?.action).toBeNull()
})

test('a missing or malformed graph reads as no nodes', () => {
  expect(evidenceNodes(undefined)).toEqual([])
  expect(evidenceNodes({})).toEqual([])
  expect(evidenceNodes({ nodes: 'x', edges: null })).toEqual([])
  expect(evidenceNodes({ nodes: [{ status: 'ok' }] })).toEqual([])
})
