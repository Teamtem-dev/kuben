/**
 * The setup token of the installer's link. It rides in the fragment
 * (`/setup#token=…`), which the browser never sends to a server nor puts in a
 * `Referer`; links printed before that carried it in the query.
 */
export function setupTokenFrom(hash: string, queryToken?: string): string | undefined {
  const fromHash = new URLSearchParams(hash.replace(/^#/, '')).get('token')?.trim()
  return fromHash || queryToken?.trim() || undefined
}

/**
 * The SSH tunnel that reaches a console on `host` from this machine, and the
 * link to open through it.
 */
export function tunnelFor(host: string, port: string, token?: string): { command: string; link: string } {
  const p = port || '80'
  return {
    command: `ssh -L ${p}:127.0.0.1:${p} <you>@${host}`,
    link: `http://localhost:${p}/setup${token ? `#token=${token}` : ''}`,
  }
}
