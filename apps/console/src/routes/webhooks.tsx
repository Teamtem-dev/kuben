import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { ChevronDownIcon, PlusIcon, SendIcon } from 'lucide-react'
import { type FormEvent, useState } from 'react'
import {
  CheckboxField,
  ConfirmDelete,
  Copyable,
  EmptyState,
  ErrorAlert,
  FormDialog,
  Loading,
  Notice,
  PageHeader,
  Section,
  Tag,
  TextInput,
  ToneBadge,
} from '@/components/kit'
import { Button } from '@/components/ui/button'
import { Collapsible, CollapsibleContent, CollapsibleTrigger } from '@/components/ui/collapsible'
import { DialogClose, DialogFooter } from '@/components/ui/dialog'
import { WEBHOOK_EVENTS, when } from '@/lib/ops'
import {
  createWebhook,
  type Delivery,
  deliveriesQuery,
  disableWebhook,
  pingWebhook,
  retryDelivery,
  type Webhook,
  webhooksQuery,
} from '@/lib/ops-api'
import { usePrefs } from '@/lib/prefs'
import { cn } from '@/lib/utils'

/** The new webhook's form; hands the signing secret (shown once) to the page. */
function CreateWebhookForm({ onCreated }: { onCreated: (secret: string | null) => void }) {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [events, setEvents] = useState<string[]>(['*'])
  const create = useMutation({
    mutationFn: (form: FormData) =>
      createWebhook(String(form.get('name') ?? ''), String(form.get('url') ?? ''), events),
    onSuccess: async (created) => {
      await queryClient.invalidateQueries({ queryKey: ['webhooks'] })
      onCreated(created.secret ?? null)
    },
  })
  const toggle = (event: string) =>
    setEvents((current) => {
      const rest = current.filter((e) => e !== '*' && e !== event)
      return current.includes(event) ? (rest.length ? rest : ['*']) : [...rest, event]
    })
  const submit = (e: FormEvent<HTMLFormElement>) => {
    e.preventDefault()
    create.mutate(new FormData(e.currentTarget))
  }
  return (
    <form onSubmit={submit} className="grid gap-4">
      <div className="grid gap-4 sm:grid-cols-2">
        <TextInput label={t('webhooks.name')} name="name" required placeholder="ops-pager" />
        <TextInput
          label={t('webhooks.url')}
          name="url"
          type="url"
          required
          dir="ltr"
          placeholder="https://"
        />
      </div>
      <fieldset className="space-y-3">
        <legend className="mb-3 font-medium text-sm">{t('webhooks.events')}</legend>
        <CheckboxField
          label={t('webhooks.allEvents')}
          checked={events.includes('*')}
          onCheckedChange={() => setEvents(['*'])}
        />
        <div className="grid gap-2 sm:grid-cols-2">
          {WEBHOOK_EVENTS.map((event) => (
            <CheckboxField
              key={event}
              label={
                <span dir="ltr" className="font-mono text-xs">
                  {event}
                </span>
              }
              checked={events.includes(event)}
              onCheckedChange={() => toggle(event)}
            />
          ))}
        </div>
      </fieldset>
      <ErrorAlert error={create.error} />
      <DialogFooter>
        <DialogClose asChild>
          <Button type="button" variant="outline">
            {t('ui.cancel')}
          </Button>
        </DialogClose>
        <Button type="submit" disabled={create.isPending}>
          {create.isPending ? t('ui.creating') : t('webhooks.create')}
        </Button>
      </DialogFooter>
    </form>
  )
}

function DeliveryRow({ webhook, delivery }: { webhook: string; delivery: Delivery }) {
  const { t, tOr, locale } = usePrefs()
  const queryClient = useQueryClient()
  const retry = useMutation({
    mutationFn: () => retryDelivery(webhook, delivery.id),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['webhooks', webhook, 'deliveries'] }),
  })
  return (
    <li className="space-y-2 py-2 text-sm">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <div className="min-w-0 space-y-0.5">
          <p className="flex flex-wrap items-center gap-2">
            <ToneBadge tone={delivery.status}>
              {tOr(`webhooks.status.${delivery.status}`, delivery.status)}
            </ToneBadge>
            <span dir="ltr" className="font-mono text-xs">
              {delivery.event}
            </span>
            {delivery.lastStatus != null && <Tag>HTTP {delivery.lastStatus}</Tag>}
          </p>
          <p className="text-muted-foreground text-xs">
            {when(delivery.createdAt, locale)} · {t('webhooks.attempts')} {delivery.attempts}
          </p>
          {delivery.lastError && (
            <p dir="ltr" className="break-all text-start text-destructive text-xs">
              {delivery.lastError}
            </p>
          )}
        </div>
        {delivery.status === 'failed' && (
          <Button variant="outline" size="sm" disabled={retry.isPending} onClick={() => retry.mutate()}>
            {t('webhooks.retry')}
          </Button>
        )}
      </div>
      <ErrorAlert error={retry.error} />
    </li>
  )
}

function Deliveries({ id }: { id: string }) {
  const { t } = usePrefs()
  const deliveries = useQuery({ ...deliveriesQuery(id), refetchInterval: 10_000 })
  if (deliveries.error) return <ErrorAlert error={deliveries.error} />
  if (!deliveries.data) return <Loading />
  if (deliveries.data.length === 0)
    return <p className="text-muted-foreground text-sm">{t('webhooks.noDeliveries')}</p>
  return (
    <ul className="divide-y rounded-md border px-3">
      {deliveries.data.map((d) => (
        <DeliveryRow key={d.id} webhook={id} delivery={d} />
      ))}
    </ul>
  )
}

function WebhookRow({ webhook }: { webhook: Webhook }) {
  const { t, locale } = usePrefs()
  const queryClient = useQueryClient()
  const [open, setOpen] = useState(false)
  const refresh = () => queryClient.invalidateQueries({ queryKey: ['webhooks'] })
  const ping = useMutation({ mutationFn: () => pingWebhook(webhook.id), onSuccess: () => setOpen(true) })
  const disable = useMutation({ mutationFn: () => disableWebhook(webhook.id), onSuccess: refresh })
  const enabled = !webhook.disabledAt
  return (
    <li className="py-4 first:pt-0 last:pb-0">
      <Collapsible open={open} onOpenChange={setOpen} className="space-y-3">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0 space-y-1.5">
            <p className="flex flex-wrap items-center gap-2">
              <span dir="auto" className="font-medium">
                {webhook.name}
              </span>
              <ToneBadge tone={enabled ? 'active' : 'closed'}>
                {enabled ? t('webhooks.enabled') : t('webhooks.disabled')}
              </ToneBadge>
              {webhook.failures > 0 && (
                <ToneBadge tone="warning">{`${t('webhooks.failures')} ${webhook.failures}`}</ToneBadge>
              )}
            </p>
            <p dir="ltr" className="break-all text-start font-mono text-muted-foreground text-xs">
              {webhook.url}
            </p>
            <p className="flex flex-wrap gap-1">
              {webhook.events.map((e) => (
                <Tag key={e}>{e === '*' ? t('webhooks.allEvents') : e}</Tag>
              ))}
            </p>
            {webhook.disabledAt && (
              <p className="text-muted-foreground text-xs">
                {t('webhooks.disabledAt')} {when(webhook.disabledAt, locale)}
              </p>
            )}
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <CollapsibleTrigger asChild>
              <Button variant="outline" size="sm">
                <ChevronDownIcon
                  aria-hidden="true"
                  className={cn('transition-transform', open && 'rotate-180')}
                />
                {open ? t('webhooks.hideDeliveries') : t('webhooks.showDeliveries')}
              </Button>
            </CollapsibleTrigger>
            {enabled && (
              <Button variant="outline" size="sm" disabled={ping.isPending} onClick={() => ping.mutate()}>
                <SendIcon aria-hidden="true" />
                {t('webhooks.ping')}
              </Button>
            )}
            {enabled && (
              <ConfirmDelete
                name={webhook.name}
                what={t('webhooks.what')}
                pending={disable.isPending}
                error={disable.error}
                onConfirm={() => disable.mutate()}
              />
            )}
          </div>
        </div>
        <ErrorAlert error={ping.error} />
        <CollapsibleContent>
          <Deliveries id={webhook.id} />
        </CollapsibleContent>
      </Collapsible>
    </li>
  )
}

/** Signed webhooks of the organization. */
export function WebhooksPage() {
  const { t } = usePrefs()
  const webhooks = useQuery(webhooksQuery)
  const [adding, setAdding] = useState(false)
  const [secret, setSecret] = useState<string | null>(null)
  return (
    <div className="space-y-6">
      <PageHeader
        title={t('webhooks.title')}
        description={t('webhooks.lead')}
        actions={
          <FormDialog
            open={adding}
            onOpenChange={setAdding}
            title={t('webhooks.add')}
            className="sm:max-w-2xl"
            trigger={
              <Button>
                <PlusIcon aria-hidden="true" />
                {t('webhooks.add')}
              </Button>
            }
          >
            <CreateWebhookForm
              onCreated={(created) => {
                setSecret(created)
                setAdding(false)
              }}
            />
          </FormDialog>
        }
      />
      {secret && (
        <Notice tone="warning">
          <span className="mb-2 block">{t('webhooks.secretOnce')}</span>
          <Copyable value={secret} />
        </Notice>
      )}
      <ErrorAlert error={webhooks.error} />
      {webhooks.isPending && <Loading />}
      {webhooks.data && webhooks.data.length === 0 && <EmptyState>{t('webhooks.empty')}</EmptyState>}
      {webhooks.data && webhooks.data.length > 0 && (
        <Section title={t('webhooks.endpoints')}>
          <ul className="divide-y">
            {webhooks.data.map((w) => (
              <WebhookRow key={w.id} webhook={w} />
            ))}
          </ul>
        </Section>
      )}
    </div>
  )
}
