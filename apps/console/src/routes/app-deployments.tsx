import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { CheckIcon, ChevronDownIcon, ChevronRightIcon, XIcon } from 'lucide-react'
import { useId, useState } from 'react'
import { ErrorAlert, Loading, Notice, Section, TextareaInput, ToneBadge } from '@/components/kit'
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
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { type Deployment, deploymentsQuery, isFinalPhase } from '@/lib/api'
import {
  type Approval,
  approvalsLeft,
  COMMENT_MAX,
  type DecideRequest,
  decideBody,
  elapsed,
  isAwaitingApproval,
} from '@/lib/delivery'
import { approvalQuery, approveDeployment, deploymentRunQuery, rejectDeployment } from '@/lib/delivery-api'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { ApiError } from '@/lib/problem'
import { cn } from '@/lib/utils'

type Where = { project: string; environment: string; app: string }

const tone = (phase: string) =>
  phase === 'succeeded' || phase === 'recovered'
    ? 'bg-success'
    : phase === 'failed' || phase === 'recoveryFailed' || phase === 'manualActionRequired'
      ? 'bg-destructive'
      : isAwaitingApproval(phase)
        ? 'bg-warning'
        : isFinalPhase(phase)
          ? 'bg-muted-foreground'
          : 'bg-primary'

function Timeline({ run }: { run: Deployment }) {
  const { tOr, locale } = usePrefs()
  const phase = (p: string) => tOr(`phase.${p}`, p)
  const time = new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit', second: '2-digit' })
  return (
    <ol className="ms-1 mt-3 space-y-2 border-s ps-4">
      {run.timeline.map((step, i) => {
        const next = run.timeline[i + 1]
        return (
          <li key={`${step.phase}-${step.at}`} className="relative text-sm">
            <span
              aria-hidden="true"
              className={cn('-start-[1.3rem] absolute top-1.5 size-2 rounded-full', tone(step.phase))}
            />
            <span className="font-medium">{phase(step.phase)}</span>{' '}
            <time dateTime={new Date(step.at).toISOString()} className="text-muted-foreground text-xs">
              {time.format(step.at)}
            </time>
            {next && <span className="text-muted-foreground text-xs"> · {elapsed(next.at - step.at)}</span>}
          </li>
        )
      })}
    </ol>
  )
}

/**
 * Approve or reject, confirmed in a dialog with an optional comment. The
 * decision carries the plan hash shown here; the dialog stays open, with the
 * API's problem text, when the API refuses it.
 */
function DecideDialog({
  kind,
  run,
  approval,
  pending,
  error,
  onDecide,
}: {
  kind: 'approve' | 'reject'
  run: Deployment
  approval: Approval
  pending: boolean
  error: unknown
  onDecide: (body: DecideRequest) => Promise<unknown>
}) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  const [comment, setComment] = useState('')
  const approve = kind === 'approve'
  const title = fill(t(approve ? 'approval.approveTitle' : 'approval.rejectTitle'), {
    revision: run.generation,
  })
  return (
    <AlertDialog
      open={open}
      onOpenChange={(next) => {
        setOpen(next)
        if (!next) setComment('')
      }}
    >
      <AlertDialogTrigger asChild>
        <Button variant={approve ? 'default' : 'outline'} size="sm">
          {approve ? <CheckIcon aria-hidden="true" /> : <XIcon aria-hidden="true" />}
          {t(approve ? 'approval.approve' : 'approval.reject')}
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            const body = decideBody(approval.planHash, comment)
            if (body)
              onDecide(body).then(
                () => setOpen(false),
                () => undefined,
              )
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{title}</AlertDialogTitle>
            <AlertDialogDescription>
              {approve
                ? fill(t('approval.approveConfirm'), { required: approval.required })
                : t('approval.rejectConfirm')}
            </AlertDialogDescription>
          </AlertDialogHeader>
          <div className="space-y-1 text-sm">
            <p className="text-muted-foreground">{t('approval.planHash')}</p>
            <code
              dir="ltr"
              className="block break-all rounded-md bg-muted px-2 py-1 text-start font-mono text-xs"
            >
              {approval.planHash}
            </code>
          </div>
          <TextareaInput
            label={t('approval.comment')}
            hint={t('approval.commentHint')}
            name="comment"
            value={comment}
            maxLength={COMMENT_MAX}
            dir="auto"
            className="[&_textarea]:font-sans"
            onChange={(e) => setComment(e.target.value)}
          />
          <ErrorAlert error={error} />
          <AlertDialogFooter>
            <AlertDialogCancel type="button">{t('ui.cancel')}</AlertDialogCancel>
            <Button type="submit" variant={approve ? 'default' : 'destructive'} disabled={pending}>
              {pending
                ? t(approve ? 'approval.approving' : 'approval.rejecting')
                : t(approve ? 'approval.approve' : 'approval.reject')}
            </Button>
          </AlertDialogFooter>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/**
 * A run waiting for approval: who asked, how many approvals it has and needs,
 * until when, the plan (revision, image, plan hash and admission's warnings),
 * the decisions so far, and Approve / Reject for someone the API lets decide.
 */
function ApprovalPanel({ project, environment, app, run }: Where & { run: Deployment }) {
  const { t, locale } = usePrefs()
  const headingId = useId()
  const queryClient = useQueryClient()
  const approval = useQuery(approvalQuery(project, environment, app, run.run))
  const detail = useQuery(deploymentRunQuery(project, environment, app, run.run))
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })

  const settle = {
    onSuccess: async (next: Approval) => {
      queryClient.setQueryData(approvalQuery(project, environment, app, run.run).queryKey, next)
      await queryClient.invalidateQueries({ queryKey: ['app', project, environment, app, 'deployments'] })
    },
    // Not waiting any more, decided meanwhile or a changed plan: show the run as it is now.
    onError: (error: Error) => {
      if (error instanceof ApiError && error.status === 409) {
        void queryClient.invalidateQueries({
          queryKey: approvalQuery(project, environment, app, run.run).queryKey,
        })
      }
    },
  }
  const approve = useMutation({
    mutationFn: (body: DecideRequest) => approveDeployment(project, environment, app, run.run, body),
    ...settle,
  })
  const reject = useMutation({
    mutationFn: (body: DecideRequest) => rejectDeployment(project, environment, app, run.run, body),
    ...settle,
  })

  if (approval.isError) return <ErrorAlert error={approval.error} className="mt-3" />
  if (!approval.data) return <Loading lines={2} className="mt-3" />
  const a = approval.data
  const warnings = detail.data?.warnings ?? []
  return (
    <section
      aria-labelledby={headingId}
      className="mt-3 space-y-3 rounded-lg border border-warning/30 bg-warning/5 p-4"
    >
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h3 id={headingId} className="font-medium text-sm">
          {t('approval.title')}
        </h3>
        <ToneBadge tone={approvalsLeft(a) > 0 ? 'warning' : 'success'}>
          {fill(t('approval.progress'), { approved: a.approved, required: a.required })}
        </ToneBadge>
      </div>
      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[auto_1fr]">
        <dt className="text-muted-foreground">{t('approval.requestedBy')}</dt>
        <dd dir="ltr" className="text-start">
          {a.requestedBy}
        </dd>
        {a.expiresAt != null && (
          <>
            <dt className="text-muted-foreground">{t('approval.expires')}</dt>
            <dd>
              <time dateTime={new Date(a.expiresAt).toISOString()}>{date.format(a.expiresAt)}</time>
            </dd>
          </>
        )}
        <dt className="text-muted-foreground">{t('approval.plan')}</dt>
        <dd className="min-w-0">
          {fill(t('approval.planRevision'), { revision: run.generation })}
          {run.image && (
            <span dir="ltr" className="block truncate text-start font-mono text-muted-foreground text-xs">
              {run.image}
            </span>
          )}
        </dd>
        {a.planHash && (
          <>
            <dt className="text-muted-foreground">{t('approval.planHash')}</dt>
            <dd className="min-w-0">
              <code dir="ltr" className="block break-all text-start font-mono text-xs">
                {a.planHash}
              </code>
              <span className="text-muted-foreground text-xs">{t('approval.planHashHint')}</span>
            </dd>
          </>
        )}
      </dl>
      {warnings.length > 0 && (
        <div className="space-y-1 text-sm">
          <p className="font-medium">{t('approval.warnings')}</p>
          <ul className="list-disc space-y-0.5 ps-5 text-warning text-xs">
            {warnings.map((w) => (
              <li key={w} dir="auto">
                {w}
              </li>
            ))}
          </ul>
        </div>
      )}
      <div className="space-y-1 text-sm">
        <p className="font-medium">{t('approval.decisions')}</p>
        {a.decisions.length === 0 ? (
          <p className="text-muted-foreground text-xs">{t('approval.noDecisions')}</p>
        ) : (
          <ul className="space-y-1">
            {a.decisions.map((d) => (
              <li
                key={`${d.approver}-${d.decidedAt}`}
                className="flex flex-wrap items-center gap-x-2 gap-y-1"
              >
                <ToneBadge tone={d.decision === 'approved' ? 'success' : 'danger'}>
                  {t(d.decision === 'approved' ? 'approval.decision.approved' : 'approval.decision.rejected')}
                </ToneBadge>
                <span dir="ltr">{d.approver}</span>
                <time
                  dateTime={new Date(d.decidedAt).toISOString()}
                  className="text-muted-foreground text-xs"
                >
                  {date.format(d.decidedAt)}
                </time>
                {d.comment && (
                  <span dir="auto" className="basis-full text-muted-foreground text-xs">
                    {d.comment}
                  </span>
                )}
              </li>
            ))}
          </ul>
        )}
      </div>
      {!a.canDecide ? (
        <p className="text-muted-foreground text-xs">{t('approval.cannotDecide')}</p>
      ) : !a.planHash ? (
        <p className="text-muted-foreground text-xs">{t('approval.noPlan')}</p>
      ) : (
        <div className="flex flex-wrap gap-2">
          <DecideDialog
            kind="approve"
            run={run}
            approval={a}
            pending={approve.isPending}
            error={approve.error}
            onDecide={(body) => {
              reject.reset()
              return approve.mutateAsync(body)
            }}
          />
          <DecideDialog
            kind="reject"
            run={run}
            approval={a}
            pending={reject.isPending}
            error={reject.error}
            onDecide={(body) => {
              approve.reset()
              return reject.mutateAsync(body)
            }}
          />
        </div>
      )}
      <ErrorAlert error={approve.error ?? reject.error} />
      {(approve.isSuccess || reject.isSuccess) && <Notice tone="success">{t('approval.decided')}</Notice>}
    </section>
  )
}

/** One run: its summary, and when open its image, its approval and its timeline. */
function Run({ run, defaultOpen, where }: { run: Deployment; defaultOpen: boolean; where: Where }) {
  const { t, tOr, locale } = usePrefs()
  const [open, setOpen] = useState(defaultOpen)
  const date = new Intl.DateTimeFormat(locale, { dateStyle: 'medium', timeStyle: 'short' })
  const Chevron = open ? ChevronDownIcon : ChevronRightIcon
  const awaiting = isAwaitingApproval(run.phase)
  return (
    <Collapsible open={open} onOpenChange={setOpen}>
      <CollapsibleTrigger className="flex w-full cursor-pointer flex-wrap items-center gap-x-3 gap-y-1 rounded-md text-start text-sm outline-none focus-visible:ring-[3px] focus-visible:ring-ring/50">
        <Chevron aria-hidden="true" className="size-4 shrink-0 text-muted-foreground rtl:-scale-x-100" />
        <span className={cn('size-2 shrink-0 rounded-full', tone(run.phase))} aria-hidden="true" />
        <span className="font-medium">
          {t('deployments.revision')} {run.generation}
        </span>
        <span className="text-muted-foreground">{tOr(`deployments.reason.${run.reason}`, run.reason)}</span>
        {awaiting ? (
          <ToneBadge tone="warning">{tOr(`phase.${run.phase}`, run.phase)}</ToneBadge>
        ) : (
          <span>{tOr(`phase.${run.phase}`, run.phase)}</span>
        )}
        <span className="text-muted-foreground text-xs">
          {date.format(run.created_at)} · {t('deployments.by')} <span dir="ltr">{run.requested_by}</span>
        </span>
      </CollapsibleTrigger>
      <CollapsibleContent className="ps-6">
        {run.image && (
          <p dir="ltr" className="mt-2 truncate text-start font-mono text-muted-foreground text-xs">
            {run.image}
          </p>
        )}
        {awaiting && <ApprovalPanel {...where} run={run} />}
        <Timeline run={run} />
      </CollapsibleContent>
    </Collapsible>
  )
}

/** The app's deployment runs, newest first, each with its phase timeline; runs waiting for approval open. */
export function DeploymentsCard({ project, environment, app }: Where) {
  const { t } = usePrefs()
  const runs = useQuery({ ...deploymentsQuery(project, environment, app), retry: false })
  const waiting = runs.data?.filter((run) => isAwaitingApproval(run.phase)).length ?? 0
  return (
    <Section title={t('deployments.title')}>
      {runs.isError ? (
        <ErrorAlert error={runs.error} />
      ) : runs.isLoading ? (
        <Loading lines={2} />
      ) : !runs.data?.length ? (
        <p className="text-muted-foreground text-sm">{t('deployments.empty')}</p>
      ) : (
        <>
          {waiting > 0 && (
            <Notice tone="warning">{fill(t('deployments.waiting'), { count: waiting })}</Notice>
          )}
          <ul className="divide-y">
            {runs.data.map((run, i) => (
              <li key={run.run} className="py-3 first:pt-0 last:pb-0">
                <Run
                  run={run}
                  defaultOpen={i === 0 || isAwaitingApproval(run.phase)}
                  where={{ project, environment, app }}
                />
              </li>
            ))}
          </ul>
        </>
      )}
    </Section>
  )
}
