/**
 * The Doctor's evidence graph (`internal/evidence`): one
 * node per layer from the build to the visitor, and edges from a cause to
 * what it affects. The API types it as a free-form object; this reads it
 * defensively and orders it for display.
 */

/** The layers, upstream first (the hub's order). */
export const LAYERS = [
  'build',
  'release',
  'deployment',
  'pods',
  'service',
  'endpoints',
  'gateway',
  'route',
  'dns',
  'tls',
] as const

export interface EvidenceNode {
  layer: string
  status: string
  subject: string
  evidence: string[]
  observedAt: number | null
  action: string | null
  /** The layers this one depends on, upstream first. */
  causes: string[]
}

const text = (v: unknown): string => (typeof v === 'string' ? v : '')
const rank = (layer: string) => {
  const i = (LAYERS as readonly string[]).indexOf(layer)
  return i < 0 ? LAYERS.length : i
}
const byLayer = (a: string, b: string) => rank(a) - rank(b)

/** The graph's nodes, upstream first, each with the layers it depends on. */
export function evidenceNodes(graph: unknown): EvidenceNode[] {
  const g = (graph ?? {}) as { nodes?: unknown; edges?: unknown }
  const nodes = Array.isArray(g.nodes) ? g.nodes : []
  const edges = Array.isArray(g.edges) ? g.edges : []
  const causes = new Map<string, string[]>()
  for (const e of edges as { from?: unknown; to?: unknown }[]) {
    const from = text(e?.from)
    const to = text(e?.to)
    if (from && to) causes.set(to, [...(causes.get(to) ?? []), from])
  }
  return (nodes as Record<string, unknown>[])
    .filter((n) => n && text(n.layer))
    .map((n) => ({
      layer: text(n.layer),
      status: text(n.status) || 'unknown',
      subject: text(n.subject),
      evidence: Array.isArray(n.evidence) ? n.evidence.filter((e): e is string => typeof e === 'string') : [],
      observedAt: typeof n.observedAt === 'number' ? n.observedAt : null,
      action: typeof n.action === 'string' && n.action !== '' ? n.action : null,
      causes: [...new Set(causes.get(text(n.layer)) ?? [])].sort(byLayer),
    }))
    .sort((a, b) => byLayer(a.layer, b.layer))
}
