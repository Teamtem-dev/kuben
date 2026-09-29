import { describe, expect, test } from 'bun:test'
import {
  absoluteUrl,
  cardNeedsUrl,
  cardOf,
  cardProvider,
  checkTone,
  connectionBody,
  connectionChange,
  lastCheckTone,
  NAME_PATTERN,
  pickBranch,
  presetNeedsServer,
  registryBody,
  registryHost,
  repositoryFrom,
  sourceBody,
  suggestName,
  viaFrom,
  viaValue,
} from './integrations'

describe('provider cards', () => {
  test('Forgejo is saved as a Gitea connection; Gitea and Forgejo need their URL', () => {
    expect(cardProvider('forgejo')).toBe('gitea')
    expect(cardProvider('gitlab')).toBe('gitlab')
    expect(cardNeedsUrl('gitea')).toBe(true)
    expect(cardNeedsUrl('forgejo')).toBe(true)
    expect(cardNeedsUrl('gitlab')).toBe(false)
  })

  test('a saved connection is shown on the card it most likely came from', () => {
    expect(cardOf({ provider: 'gitlab', baseUrl: 'https://gitlab.com' })).toBe('gitlab')
    expect(cardOf({ provider: 'gitea', baseUrl: 'https://codeberg.org' })).toBe('forgejo')
    expect(cardOf({ provider: 'gitea', baseUrl: 'https://git.example.com' })).toBe('gitea')
  })

  test('the suggested name is the card, plus the host of a self-hosted server', () => {
    const valid = new RegExp(`^${NAME_PATTERN}$`)
    expect(suggestName('gitlab', 'https://gitlab.com')).toBe('gitlab')
    expect(suggestName('github', 'https://api.github.com')).toBe('github')
    expect(suggestName('gitea', '')).toBe('gitea')
    const named = suggestName('gitlab', 'https://GitLab.Example.com:8443/')
    expect(named).toBe('gitlab-gitlab-example-com')
    expect(valid.test(named)).toBe(true)
    expect(suggestName('forgejo', 'not a url')).toBe('forgejo')
  })
})

describe('connection bodies', () => {
  test('a create sends the provider, trimmed values and nothing empty', () => {
    expect(
      connectionBody({
        card: 'forgejo',
        name: ' codeberg ',
        url: 'https://codeberg.org/',
        token: ' tok ',
        defaultBranch: '',
      }),
    ).toEqual({ provider: 'gitea', name: 'codeberg', token: 'tok', baseUrl: 'https://codeberg.org' })
    expect(
      connectionBody({ card: 'gitlab', name: 'gl', url: '', token: 't', defaultBranch: 'main' }),
    ).toEqual({ provider: 'gitlab', name: 'gl', token: 't', defaultBranch: 'main' })
  })

  test('an edit sends only what changed; an empty token keeps it, an empty branch clears it', () => {
    const saved = { name: 'gl', baseUrl: 'https://gitlab.com', defaultBranch: 'main' }
    expect(
      connectionChange(saved, { name: 'gl', url: 'https://gitlab.com', token: '', defaultBranch: 'main' }),
    ).toEqual({})
    expect(
      connectionChange(saved, {
        name: 'gl-2',
        url: 'https://gitlab.com/',
        token: ' new ',
        defaultBranch: '',
      }),
    ).toEqual({ name: 'gl-2', token: 'new', defaultBranch: null })
  })

  test('a webhook path becomes an address on the console origin', () => {
    expect(absoluteUrl('/api/v1/webhooks/gitlab/1', 'https://kuben.example.com/')).toBe(
      'https://kuben.example.com/api/v1/webhooks/gitlab/1',
    )
    expect(absoluteUrl('https://hooks.example.com/x', 'https://kuben.example.com')).toBe(
      'https://hooks.example.com/x',
    )
  })
})

describe('checks', () => {
  test('a check is fine, fine with scopes missing, or failed', () => {
    expect(checkTone({ ok: true, missingScopes: [] })).toBe('success')
    expect(checkTone({ ok: true, missingScopes: ['read_api'] })).toBe('warning')
    expect(checkTone({ ok: false, missingScopes: [] })).toBe('danger')
  })

  test('a saved item was never checked, works, or failed last time', () => {
    expect(lastCheckTone({})).toBe('neutral')
    expect(lastCheckTone({ lastCheckedAt: 1 })).toBe('success')
    expect(lastCheckTone({ lastCheckedAt: 1, lastError: 'unauthorized' })).toBe('danger')
  })
})

describe('registries', () => {
  const ghcr = { id: 'ghcr' as const, label: 'GitHub', server: 'ghcr.io', usernameHint: '', passwordHint: '' }
  const harbor = { id: 'harbor' as const, label: 'Harbor', server: null, usernameHint: '', passwordHint: '' }

  test('a server is named as image references name it', () => {
    expect(registryHost(' https://Registry.Example.com:5000/v2/ ')).toBe('registry.example.com:5000')
    expect(registryHost('ghcr.io')).toBe('ghcr.io')
  })

  test('the server is sent when the preset has none or it differs from the preset', () => {
    expect(presetNeedsServer(harbor)).toBe(true)
    expect(presetNeedsServer(ghcr)).toBe(false)
    const form = {
      preset: 'ghcr' as const,
      name: ' ghcr ',
      server: 'ghcr.io',
      username: ' me ',
      password: 'p',
    }
    expect(registryBody(form, ghcr)).toEqual({ preset: 'ghcr', name: 'ghcr', username: 'me', password: 'p' })
    expect(
      registryBody({ ...form, preset: 'harbor', server: 'https://harbor.example.com/' }, harbor),
    ).toEqual({ preset: 'harbor', name: 'ghcr', username: 'me', password: 'p', server: 'harbor.example.com' })
  })
})

describe('the app source', () => {
  test('the source select round-trips a connection or an installation', () => {
    expect(viaFrom(viaValue({ kind: 'connection', id: 'a:b' }))).toEqual({ kind: 'connection', id: 'a:b' })
    expect(viaFrom(viaValue({ kind: 'installation', id: 42 }))).toEqual({ kind: 'installation', id: 42 })
    expect(viaFrom('installation:x')).toBeNull()
    expect(viaFrom('other:1')).toBeNull()
  })

  test('a repository is owner/name, from a name or a URL', () => {
    expect(repositoryFrom('acme/shop')).toBe('acme/shop')
    expect(repositoryFrom('https://gitlab.com/group/sub/shop.git')).toBe('group/sub/shop')
    expect(repositoryFrom('shop')).toBeNull()
    expect(repositoryFrom('acme / shop')).toBeNull()
  })

  test('PUT source names the connection or the installation, and a Dockerfile only for that strategy', () => {
    const base = {
      repository: ' acme/shop ',
      branch: 'main',
      strategy: 'railpack' as const,
      context: '',
      dockerfile: 'Dockerfile.prod',
      imageRepository: 'registry.example.com/acme/shop',
    }
    expect(sourceBody({ ...base, via: { kind: 'connection', id: 'c1' } })).toEqual({
      repository: 'acme/shop',
      branch: 'main',
      strategy: 'railpack',
      imageRepository: 'registry.example.com/acme/shop',
      connection: 'c1',
    })
    expect(
      sourceBody({
        ...base,
        strategy: 'dockerfile',
        context: 'apps/web',
        via: { kind: 'installation', id: 7 },
      }),
    ).toEqual({
      repository: 'acme/shop',
      branch: 'main',
      strategy: 'dockerfile',
      imageRepository: 'registry.example.com/acme/shop',
      installationId: 7,
      context: 'apps/web',
      dockerfile: 'Dockerfile.prod',
    })
  })

  test('the branch picker keeps a choice, else takes the default, the fallback or the first', () => {
    const branches = [
      { name: 'dev', protected: false, default: false },
      { name: 'main', protected: true, default: true },
    ]
    expect(pickBranch(branches, 'dev')).toBe('dev')
    expect(pickBranch(branches, 'gone')).toBe('main')
    expect(
      pickBranch([{ name: 'a', protected: false, default: false }, ...branches.slice(0, 1)], '', 'dev'),
    ).toBe('dev')
    expect(pickBranch([{ name: 'a', protected: false, default: false }], '')).toBe('a')
    expect(pickBranch([], '')).toBe('')
  })
})
