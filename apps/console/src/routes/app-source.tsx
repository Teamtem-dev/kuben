/**
 * The app's Git source, on its settings tab: where it builds from (provider,
 * connection, repository, branch, recipe) and the "connect a repository"
 * dialog: a connection or a GitHub App installation, then a repository, a
 * branch and how to build it.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { Link } from '@tanstack/react-router'
import { GitBranchIcon } from 'lucide-react'
import { type FormEvent, type ReactNode, useState } from 'react'
import { BranchPicker, ProviderMark, RepositoryPicker } from '@/components/git'
import {
  ErrorAlert,
  FormDialog,
  Loading,
  Notice,
  Section,
  SelectInput,
  Tag,
  TextInput,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import {
  type AppSource,
  cardOf,
  type GitConnection,
  type Installation,
  repositoryFrom,
  type SourceForm,
  type SourceVia,
  type Strategy,
  sourceBody,
  viaFrom,
  viaValue,
} from '@/lib/integrations'
import {
  appSourceQuery,
  gitConnectionsQuery,
  gitInstallationsQuery,
  putAppSource,
} from '@/lib/integrations-api'
import { usePrefs } from '@/lib/prefs'

type Where = { project: string; environment: string; app: string }

const STRATEGIES: readonly Strategy[] = ['auto', 'dockerfile', 'railpack']

/** The form's first values: the saved source, or the first way in. */
function initialForm(
  source: AppSource | null,
  connections: readonly GitConnection[],
  installations: readonly Installation[],
): SourceForm | null {
  const via: SourceVia | null = source
    ? source.connection
      ? { kind: 'connection', id: source.connection }
      : { kind: 'installation', id: source.installationId }
    : connections[0]
      ? { kind: 'connection', id: connections[0].id }
      : installations[0]
        ? { kind: 'installation', id: installations[0].installationId }
        : null
  if (!via) return null
  return {
    via,
    repository: source?.repository ?? '',
    branch: source?.branch ?? '',
    strategy: source?.strategy ?? 'auto',
    context: source?.context ?? '',
    dockerfile: source?.dockerfile ?? '',
    imageRepository: source?.imageRepository ?? '',
  }
}

function ConnectForm({
  project,
  environment,
  app,
  source,
  connections,
  installations,
  onDone,
}: Where & {
  source: AppSource | null
  connections: readonly GitConnection[]
  installations: readonly Installation[]
  onDone: () => void
}) {
  const { t, tOr } = usePrefs()
  const queryClient = useQueryClient()
  const [form, setForm] = useState<SourceForm | null>(() => initialForm(source, connections, installations))
  const [typedRepository, setTypedRepository] = useState(source?.repository ?? '')
  // Said once the person tried to save a repository that is not owner/name.
  const [badRepository, setBadRepository] = useState(false)
  const save = useMutation({
    mutationFn: (body: SourceForm) => putAppSource(project, environment, app, sourceBody(body)),
    onSuccess: async (next) => {
      queryClient.setQueryData(appSourceQuery(project, environment, app).queryKey, next)
      await queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })
      onDone()
    },
  })
  if (!form) {
    return (
      <div className="grid gap-4">
        <Notice>
          {t('source.noWay')}{' '}
          <Link to="/settings/integrations" className="text-link hover:underline">
            {t('nav.integrations')}
          </Link>
        </Notice>
        <DialogFooter>
          <DialogClose asChild>
            <Button type="button" variant="outline">
              {t('git.close')}
            </Button>
          </DialogClose>
        </DialogFooter>
      </div>
    )
  }
  const set = (patch: Partial<SourceForm>) => setForm({ ...form, ...patch })
  const viaConnection = form.via.kind === 'connection' ? form.via.id : null
  const connection = connections.find((c) => c.id === viaConnection)
  const setBranch = (branch: string) => setForm((f) => (f ? { ...f, branch } : f))

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    if (!form) return
    const repository = form.via.kind === 'installation' ? repositoryFrom(typedRepository) : form.repository
    setBadRepository(form.via.kind === 'installation' && repository == null)
    if (!repository || !form.branch.trim()) return
    save.mutate({ ...form, repository })
  }

  return (
    <form onSubmit={onSubmit} className="grid gap-4">
      <SelectInput
        label={t('source.via')}
        value={viaValue(form.via)}
        onChange={(e) => {
          const via = viaFrom(e.target.value)
          if (via) set({ via, repository: '', branch: '' })
        }}
      >
        {connections.length > 0 && (
          <optgroup label={t('git.connections')}>
            {connections.map((c) => (
              <option key={c.id} value={viaValue({ kind: 'connection', id: c.id })}>
                {c.name} ({t(`git.card.${cardOf(c)}`)})
              </option>
            ))}
          </optgroup>
        )}
        {installations.length > 0 && (
          <optgroup label={t('git.githubApp')}>
            {installations.map((i) => (
              <option
                key={i.installationId}
                value={viaValue({ kind: 'installation', id: i.installationId })}
                disabled={i.suspended}
              >
                {i.account} (#{i.installationId})
              </option>
            ))}
          </optgroup>
        )}
      </SelectInput>

      {connection ? (
        <>
          <RepositoryPicker
            key={connection.id}
            connection={connection.id}
            value={form.repository}
            onChange={(r) => set({ repository: r.fullName, branch: '' })}
          />
          {form.repository && (
            <BranchPicker
              key={`${connection.id} ${form.repository}`}
              connection={connection.id}
              repository={form.repository}
              value={form.branch}
              fallback={connection.defaultBranch}
              onChange={setBranch}
            />
          )}
        </>
      ) : (
        <div className="grid gap-4 sm:grid-cols-2">
          <TextInput
            label={t('source.repository')}
            dir="ltr"
            required
            placeholder="acme/shop"
            value={typedRepository}
            aria-invalid={badRepository || undefined}
            hint={
              badRepository ? (
                <span role="alert" className="text-destructive">
                  {t('source.repositoryInvalid')}
                </span>
              ) : (
                t('source.repositoryHint')
              )
            }
            onChange={(e) => {
              setTypedRepository(e.target.value)
              setBadRepository(false)
            }}
          />
          <TextInput
            label={t('git.branch')}
            dir="ltr"
            required
            placeholder="main"
            value={form.branch}
            onChange={(e) => set({ branch: e.target.value })}
          />
        </div>
      )}

      <fieldset className="grid gap-4 sm:grid-cols-2">
        <legend className="mb-2 font-medium text-sm">{t('source.recipe')}</legend>
        <SelectInput
          label={t('builds.strategy')}
          value={form.strategy}
          onChange={(e) => set({ strategy: e.target.value as Strategy })}
          hint={tOr(`source.strategyHint.${form.strategy}`, '')}
        >
          {STRATEGIES.map((s) => (
            <option key={s} value={s}>
              {t(`source.strategy.${s}`)}
            </option>
          ))}
        </SelectInput>
        <TextInput
          label={t('source.context')}
          dir="ltr"
          placeholder="apps/web"
          value={form.context}
          hint={t('source.contextHint')}
          onChange={(e) => set({ context: e.target.value })}
        />
        {form.strategy === 'dockerfile' && (
          <TextInput
            label={t('source.dockerfile')}
            dir="ltr"
            placeholder="Dockerfile"
            value={form.dockerfile}
            onChange={(e) => set({ dockerfile: e.target.value })}
          />
        )}
        <TextInput
          label={t('source.imageRepository')}
          dir="ltr"
          required
          placeholder="registry.example.com/acme/shop"
          value={form.imageRepository}
          hint={t('source.imageRepositoryHint')}
          onChange={(e) => set({ imageRepository: e.target.value })}
        />
      </fieldset>

      <ErrorAlert error={save.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button
          type="submit"
          disabled={save.isPending || (form.via.kind === 'connection' && (!form.repository || !form.branch))}
        >
          {save.isPending ? t('ui.saving') : t('source.save')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function Row({ label, children }: { label: string; children: ReactNode }) {
  return (
    <>
      <dt className="text-muted-foreground">{label}</dt>
      <dd className="min-w-0">{children}</dd>
    </>
  )
}

/** Where the app builds from, and the dialog that connects or changes it. */
export function SourceCard({ project, environment, app }: Where) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  const source = useQuery(appSourceQuery(project, environment, app))
  const connections = useQuery(gitConnectionsQuery)
  const installations = useQuery(gitInstallationsQuery)
  const s = source.data
  const connection = s?.connection ? connections.data?.find((c) => c.id === s.connection) : undefined
  const card = connection ? cardOf(connection) : (s?.provider ?? 'github')
  const ready = !connections.isPending && !installations.isPending

  return (
    <Section
      title={t('source.title')}
      description={s ? t('source.lead') : t('source.leadNone')}
      actions={
        <FormDialog
          open={open}
          onOpenChange={setOpen}
          title={s ? t('source.change') : t('source.connect')}
          description={t('source.dialogHint')}
          className="sm:max-w-2xl"
          trigger={
            <Button size="sm" variant={s ? 'outline' : 'default'} disabled={source.isPending}>
              <GitBranchIcon aria-hidden="true" />
              {s ? t('source.change') : t('source.connect')}
            </Button>
          }
        >
          {!open ? null : !ready ? (
            <Loading lines={3} />
          ) : (
            <ConnectForm
              project={project}
              environment={environment}
              app={app}
              source={s ?? null}
              connections={connections.data ?? []}
              installations={installations.data ?? []}
              onDone={() => setOpen(false)}
            />
          )}
        </FormDialog>
      }
    >
      {source.isError ? (
        <ErrorAlert error={source.error} />
      ) : source.isPending ? (
        <Loading lines={3} />
      ) : !s ? null : (
        <dl className="grid gap-x-6 gap-y-2 text-sm sm:grid-cols-[max-content_1fr]">
          <Row label={t('git.provider')}>
            <span className="flex items-center gap-2">
              <ProviderMark card={card} className="size-6" />
              {t(`git.card.${card}`)}
            </span>
          </Row>
          <Row label={t('git.connection')}>
            {s.connection ? (
              <span dir="ltr" className="font-mono text-xs">
                {connection?.name ?? s.connection}
              </span>
            ) : (
              <span>
                {t('git.githubApp')} <Tag>#{s.installationId}</Tag>
              </span>
            )}
          </Row>
          <Row label={t('source.repository')}>
            <span dir="ltr" className="block text-start font-mono text-xs">
              {s.repository}
            </span>
          </Row>
          <Row label={t('git.branch')}>
            <span dir="ltr" className="block text-start font-mono text-xs">
              {s.branch}
            </span>
          </Row>
          <Row label={t('builds.strategy')}>
            <Tag>{s.strategy}</Tag>
            {s.context && (
              <span dir="ltr" className="ms-2 font-mono text-muted-foreground text-xs">
                {s.context}
                {s.dockerfile ? `/${s.dockerfile}` : ''}
              </span>
            )}
          </Row>
          <Row label={t('source.imageRepository')}>
            <span dir="ltr" className="block break-all text-start font-mono text-xs">
              {s.imageRepository}
            </span>
          </Row>
          {s.head && (
            <Row label={t('source.head')}>
              <span dir="ltr" className="block break-all text-start font-mono text-xs">
                {s.head}
              </span>
            </Row>
          )}
        </dl>
      )}
    </Section>
  )
}
