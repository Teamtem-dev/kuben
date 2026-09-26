/**
 * Operational controls (M4.9): who answers for a project or an app, change
 * freezes and alert silences of an environment, holding an app's delivery,
 * and the emergency rollback.
 */
import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ExternalLinkIcon, PauseIcon, PlayIcon, PlusIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  ConfirmAction,
  EmptyState,
  ErrorAlert,
  FormDialog,
  Loading,
  Notice,
  Section,
  SelectInput,
  SwitchField,
  TextareaInput,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import {
  AlertDialog,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from '@/components/ui/alert-dialog'
import { Button } from '@/components/ui/button'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { localInput, rfc3339, type WindowState, windowState } from '@/lib/controls'
import {
  appOwnerQuery,
  type ControlWindow,
  createFreeze,
  createSilence,
  emergencyRollback,
  freezesQuery,
  liftFreeze,
  liftSilence,
  type Owner,
  pauseApp,
  projectOwnerQuery,
  putAppOwner,
  putProjectOwner,
  resumeApp,
  silencesQuery,
} from '@/lib/controls-api'
import { fill } from '@/lib/messages/pages'
import { type Tone, when } from '@/lib/ops'
import { usePrefs } from '@/lib/prefs'

// ---- owners ----

/** Who answers for a project, or for one of its apps (in every environment). */
export function OwnerCard({ project, app }: { project: string; app?: string }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const query = app ? appOwnerQuery(project, app) : projectOwnerQuery(project)
  const owner = useQuery(query)
  const save = useMutation({
    mutationFn: (body: Owner) => (app ? putAppOwner(project, app, body) : putProjectOwner(project, body)),
    onSuccess: (saved) => queryClient.setQueryData(query.queryKey, saved),
  })
  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    const field = (key: string) => String(form.get(key) ?? '').trim()
    save.mutate({
      owner: field('owner'),
      contact: field('contact') || null,
      runbookUrl: field('runbook') || null,
    })
  }
  const current = owner.data
  return (
    <Section title={t('owner.title')} description={app ? t('owner.appHint') : t('owner.projectHint')}>
      {owner.isError ? (
        <ErrorAlert error={owner.error} />
      ) : owner.isPending ? (
        <Loading lines={2} />
      ) : (
        <form key={current?.owner ?? 'none'} onSubmit={onSubmit} className="grid gap-4">
          {!current && <p className="text-muted-foreground text-sm">{t('owner.none')}</p>}
          <div className="grid gap-4 sm:grid-cols-3">
            <TextInput
              label={t('owner.owner')}
              name="owner"
              required
              maxLength={256}
              dir="auto"
              defaultValue={current?.owner ?? ''}
              placeholder="payments-team"
            />
            <TextInput
              label={t('owner.contact')}
              name="contact"
              maxLength={512}
              dir="auto"
              defaultValue={current?.contact ?? ''}
              placeholder="#payments-oncall"
            />
            <TextInput
              label={t('owner.runbook')}
              name="runbook"
              type="url"
              dir="ltr"
              defaultValue={current?.runbookUrl ?? ''}
              placeholder="https://runbooks.example.com/payments"
            />
          </div>
          {current?.runbookUrl && (
            <a
              href={current.runbookUrl}
              target="_blank"
              rel="noopener noreferrer"
              className="inline-flex w-fit items-center gap-1 text-link text-sm hover:underline"
            >
              {t('incidents.runbook')}
              <ExternalLinkIcon aria-hidden="true" className="size-3.5" />
            </a>
          )}
          <ErrorAlert error={save.error} />
          <div>
            <Button type="submit" disabled={save.isPending}>
              {save.isPending ? t('ui.saving') : t('ui.save')}
            </Button>
          </div>
        </form>
      )}
    </Section>
  )
}

// ---- freezes and silences ----

const WINDOW_TONES: Record<WindowState, Tone> = {
  active: 'warning',
  scheduled: 'neutral',
  lifted: 'neutral',
  ended: 'neutral',
}

type WindowKind = 'freeze' | 'silence'

function WindowForm({
  kind,
  project,
  environment,
  apps,
  onDone,
}: {
  kind: WindowKind
  project: string
  environment: string
  apps: readonly string[]
  onDone: () => void
}) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [defaults] = useState(() => ({ ends: localInput(Date.now() + 2 * 3_600_000) }))
  const create = useMutation({
    mutationFn: (form: FormData) => {
      const reason = String(form.get('reason') ?? '').trim()
      const endsAt = rfc3339(String(form.get('ends') ?? '')) ?? ''
      if (kind === 'freeze') {
        return createFreeze(project, environment, {
          reason,
          endsAt,
          startsAt: rfc3339(String(form.get('starts') ?? '')),
        })
      }
      return createSilence(project, environment, {
        reason,
        endsAt,
        app: String(form.get('app') ?? '') || null,
      })
    },
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: [kind === 'freeze' ? 'freezes' : 'silences', project] })
      onDone()
    },
  })
  return (
    <form
      className="grid gap-4"
      onSubmit={(event) => {
        event.preventDefault()
        create.mutate(new FormData(event.currentTarget))
      }}
    >
      <TextInput label={t('controls.reason')} name="reason" required maxLength={1024} dir="auto" />
      <div className="grid gap-4 sm:grid-cols-2">
        {kind === 'freeze' && (
          <TextInput
            label={t('controls.startsAt')}
            name="starts"
            type="datetime-local"
            hint={t('controls.startsNow')}
          />
        )}
        <TextInput
          label={t('controls.endsAt')}
          name="ends"
          type="datetime-local"
          required
          defaultValue={defaults.ends}
          hint={kind === 'freeze' ? t('controls.freezeMax') : t('controls.silenceMax')}
        />
        {kind === 'silence' && apps.length > 0 && (
          <SelectInput label={t('controls.scope')} name="app" defaultValue="">
            <option value="">{t('controls.wholeEnvironment')}</option>
            {apps.map((a) => (
              <option key={a} value={a}>
                {a}
              </option>
            ))}
          </SelectInput>
        )}
      </div>
      <ErrorAlert error={create.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          {kind === 'freeze' ? t('controls.freeze') : t('controls.silence')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function WindowRow({
  kind,
  project,
  environment,
  window: w,
}: {
  kind: WindowKind
  project: string
  environment: string
  window: ControlWindow
}) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const lift = useMutation({
    mutationFn: () =>
      kind === 'freeze' ? liftFreeze(project, environment, w.id) : liftSilence(project, environment, w.id),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: [kind === 'freeze' ? 'freezes' : 'silences', project] }),
  })
  const state = windowState(w, Date.now())
  return (
    <li className="flex flex-wrap items-start justify-between gap-3 py-3 first:pt-0 last:pb-0">
      <div className="min-w-0 space-y-1 text-sm">
        <p className="flex flex-wrap items-center gap-2">
          <ToneBadge tone={WINDOW_TONES[state]}>{t(`controls.state.${state}`)}</ToneBadge>
          {w.app && <ToneBadge tone="neutral">{t('controls.oneApp')}</ToneBadge>}
          <span dir="auto" className="font-medium">
            {w.reason}
          </span>
        </p>
        <p className="text-muted-foreground text-xs">
          {when(w.startsAt, locale)} → {when(w.endsAt, locale)} · <span dir="ltr">{w.createdBy}</span>
          {w.liftedAt && ` · ${t('controls.liftedAt')} ${when(w.liftedAt, locale)}`}
        </p>
      </div>
      {(state === 'active' || state === 'scheduled') && (
        <ConfirmAction
          size="sm"
          variant="outline"
          label={t('controls.lift')}
          title={kind === 'freeze' ? t('controls.liftFreeze') : t('controls.liftSilence')}
          description={fill(t('controls.liftConfirm'), { reason: w.reason })}
          pending={lift.isPending}
          error={lift.error}
          onConfirm={() => lift.mutateAsync()}
        />
      )}
    </li>
  )
}

/** An environment's change freezes or alert silences, and a way to add one. */
export function WindowsCard({
  kind,
  project,
  environment,
  apps = [],
}: {
  kind: WindowKind
  project: string
  environment: string
  apps?: readonly string[]
}) {
  const { t } = usePrefs()
  const [all, setAll] = useState(false)
  const [adding, setAdding] = useState(false)
  const windows = useQuery(
    kind === 'freeze' ? freezesQuery(project, environment, all) : silencesQuery(project, environment, all),
  )
  const title = kind === 'freeze' ? t('controls.freezes') : t('controls.silences')
  const add = kind === 'freeze' ? t('controls.addFreeze') : t('controls.addSilence')
  return (
    <Section
      title={title}
      description={kind === 'freeze' ? t('controls.freezesHint') : t('controls.silencesHint')}
      actions={
        <FormDialog
          open={adding}
          onOpenChange={setAdding}
          title={add}
          trigger={
            <Button size="sm" variant="outline">
              <PlusIcon aria-hidden="true" />
              {add}
            </Button>
          }
        >
          <WindowForm
            kind={kind}
            project={project}
            environment={environment}
            apps={apps}
            onDone={() => setAdding(false)}
          />
        </FormDialog>
      }
    >
      <SwitchField label={t('controls.showPast')} checked={all} onCheckedChange={setAll} />
      <ErrorAlert error={windows.error} />
      {windows.isPending && <Loading lines={2} />}
      {windows.data?.length === 0 && (
        <EmptyState>{kind === 'freeze' ? t('controls.noFreezes') : t('controls.noSilences')}</EmptyState>
      )}
      {windows.data && windows.data.length > 0 && (
        <ul className="divide-y">
          {windows.data.map((w) => (
            <WindowRow key={w.id} kind={kind} project={project} environment={environment} window={w} />
          ))}
        </ul>
      )}
    </Section>
  )
}

// ---- delivery ----

/** Hold an app's delivery (runs wait), or let the newest waiting run through. */
export function DeliveryCard({
  project,
  environment,
  app,
  paused,
}: {
  project: string
  environment: string
  app: string
  paused: string | null | undefined
}) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [pausing, setPausing] = useState(false)
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })
  const pause = useMutation({
    mutationFn: (reason: string) => pauseApp(project, environment, app, reason),
    onSuccess: async () => {
      await refresh()
      setPausing(false)
    },
  })
  const resume = useMutation({ mutationFn: () => resumeApp(project, environment, app), onSuccess: refresh })
  const isPaused = paused != null
  return (
    <Section
      title={t('delivery.title')}
      description={t('delivery.hint')}
      actions={
        isPaused ? (
          <Button variant="outline" disabled={resume.isPending} onClick={() => resume.mutate()}>
            <PlayIcon aria-hidden="true" />
            {t('delivery.resume')}
          </Button>
        ) : (
          <FormDialog
            open={pausing}
            onOpenChange={setPausing}
            title={t('delivery.pause')}
            description={t('delivery.pauseHint')}
            trigger={
              <Button variant="outline">
                <PauseIcon aria-hidden="true" />
                {t('delivery.pause')}
              </Button>
            }
          >
            <form
              className="grid gap-4"
              onSubmit={(event) => {
                event.preventDefault()
                pause.mutate(String(new FormData(event.currentTarget).get('reason') ?? '').trim())
              }}
            >
              <TextInput label={t('controls.reason')} name="reason" required maxLength={1024} dir="auto" />
              <ErrorAlert error={pause.error} />
              <DialogFooter>
                <DialogClose asChild>
                  <Button type="button" variant="outline">
                    {t('ui.cancel')}
                  </Button>
                </DialogClose>
                <Button type="submit" disabled={pause.isPending}>
                  {t('delivery.pause')}
                </Button>
              </DialogFooter>
            </form>
          </FormDialog>
        )
      }
    >
      <p className="text-sm">
        {isPaused ? (
          <>
            <ToneBadge tone="warning">{t('delivery.paused')}</ToneBadge>{' '}
            <span dir="auto" className="text-muted-foreground">
              {paused}
            </span>
          </>
        ) : (
          <ToneBadge tone="success">{t('delivery.flowing')}</ToneBadge>
        )}
      </p>
      <ErrorAlert error={resume.error} />
    </Section>
  )
}

/** Break glass: back to the last good release now, past approvals, a freeze, the scan gate and a pause. */
export function EmergencyRollbackCard({
  project,
  environment,
  app,
}: {
  project: string
  environment: string
  app: string
}) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [reason, setReason] = useState('')
  const [open, setOpen] = useState(false)
  const rollback = useMutation({
    mutationFn: () => emergencyRollback(project, environment, app, reason.trim()),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ['app', project, environment, app] })
      setOpen(false)
      setReason('')
    },
  })
  return (
    <Section
      tone="danger"
      title={t('emergency.title')}
      description={t('emergency.hint')}
      actions={
        <AlertDialog open={open} onOpenChange={setOpen}>
          <AlertDialogTrigger asChild>
            <Button variant="destructive">{t('emergency.open')}</Button>
          </AlertDialogTrigger>
          <AlertDialogContent>
            <form
              className="grid gap-4"
              onSubmit={(event) => {
                event.preventDefault()
                if (reason.trim()) rollback.mutate()
              }}
            >
              <AlertDialogHeader>
                <AlertDialogTitle>{t('emergency.title')}</AlertDialogTitle>
                <AlertDialogDescription>{t('emergency.confirm')}</AlertDialogDescription>
              </AlertDialogHeader>
              <TextareaInput
                label={t('emergency.reason')}
                value={reason}
                required
                maxLength={1024}
                dir="auto"
                className="[&_textarea]:font-sans"
                onChange={(e) => setReason(e.target.value)}
              />
              <ErrorAlert error={rollback.error} />
              <AlertDialogFooter>
                <AlertDialogCancel type="button">{t('ui.cancel')}</AlertDialogCancel>
                <Button type="submit" variant="destructive" disabled={!reason.trim() || rollback.isPending}>
                  {t('emergency.submit')}
                </Button>
              </AlertDialogFooter>
            </form>
          </AlertDialogContent>
        </AlertDialog>
      }
    >
      {rollback.isSuccess && <Notice tone="success">{t('emergency.started')}</Notice>}
    </Section>
  )
}
