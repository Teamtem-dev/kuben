import type { EnvVar } from './api'

const NAME = /^[A-Za-z_][A-Za-z0-9_]*$/
const SECRET_REF = /^@([a-z0-9]([-a-z0-9]*[a-z0-9])?)\/([-._a-zA-Z0-9]+)$/

/**
 * Parse `KEY=value` lines. `KEY=@secret/key` references a secret instead of
 * inlining the value. Blank lines and `#` comments are ignored.
 */
export function parseEnvLines(text: string): { vars: EnvVar[]; errors: string[] } {
  const vars: EnvVar[] = []
  const errors: string[] = []
  const seen = new Set<string>()
  text.split('\n').forEach((raw, index) => {
    const line = raw.trim()
    if (!line || line.startsWith('#')) return
    const eq = line.indexOf('=')
    const name = eq === -1 ? line : line.slice(0, eq).trim()
    const value = eq === -1 ? '' : line.slice(eq + 1)
    if (!NAME.test(name)) {
      errors.push(`line ${index + 1}: "${name}" is not a valid variable name`)
      return
    }
    if (seen.has(name)) {
      errors.push(`line ${index + 1}: ${name} is set twice`)
      return
    }
    seen.add(name)
    const ref = SECRET_REF.exec(value)
    if (ref?.[1] && ref[3]) vars.push({ name, secret: { name: ref[1], key: ref[3] } })
    else vars.push({ name, value })
  })
  return { vars, errors }
}

/** Inverse of {@link parseEnvLines}; values the caller may not see stay empty. */
export function formatEnvLines(vars: readonly EnvVar[]): string {
  return vars
    .map((v) => (v.secret ? `${v.name}=@${v.secret.name}/${v.secret.key}` : `${v.name}=${v.value ?? ''}`))
    .join('\n')
}
