import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { type FormEvent, useState } from 'react'
import { Copyable, Pill } from '../components/ops'
import { Badge, Button, Card, ConfirmDelete, Empty, ErrorNote, PageHeader, TextField } from '../components/ui'
import { WEBHOOK_EVENTS, when } from '../lib/ops'
import {
  createWebhook,
  type Delivery,
  deliveriesQuery,
  disableWebhook,
  pingWebhook,
  retryDelivery,
  type Webhook,
  webhooksQuery,
} from '../lib/ops-api'
import { usePrefs } from '../lib/prefs'

function CreateWebhook() {
  const { t } = usePrefs()
  const queryClient = useQueryClient()
  const [events, setEvents] = useState<string[]>(['*'])
  const create = useMutation({
    mutationFn: (form: FormData) =>
      createWebhook(String(form.get('name') ?? ''), String(form.get('url') ?? ''), events),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ['webhooks'] }),
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
    <Card title={t('webhooks.add')}>
      <form onSubmit={submit} className="space-y-4">
        <div className="grid gap-4 sm:grid-cols-2">
          <TextField label={t('webhooks.name')} name="name" required placeholder="ops-pager" />
          <TextField
            label={t('webhooks.url')}
            name="url"
            type="url"
            required
            dir="ltr"
            placeholder="https://"
          />
        </div>
        <fieldset className="space-y-2">
          <legend className="font-medium text-sm">{t('webhooks.events')}</legend>
          <label className="flex items-center gap-2 text-sm">
            <input type="checkbox" checked={events.includes('*')} onChange={() => setEvents(['*'])} />
            {t('webhooks.allEvents')}
          </label>
          <div className="grid gap-1 sm:grid-cols-2">
            {WEBHOOK_EVENTS.map((event) => (
              <label key={event} className="flex items-center gap-2 text-sm">
                <input type="checkbox" checked={events.includes(event)} onChange={() => toggle(event)} />
                <span dir="ltr" className="font-mono text-xs">
                  {event}
                </span>
              </label>
            ))}
          </div>
        </fieldset>
        <ErrorNote error={create.error} />
        {create.data?.secret && (
          <div role="status" className="space-y-2 rounded-lg border border-warn/30 bg-warn/10 p-3 text-sm">
            <p>{t('webhooks.secretOnce')}</p>
            <Copyable value={create.data.secret} label={t('ops.copy')} />
          </div>
        )}
        <Button type="submit" disabled={create.isPending}>
          {t('webhooks.create')}
        </Button>
      </form>
    </Card>
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
    <li className="flex flex-wrap items-center justify-between gap-2 py-2 text-sm">
      <div className="min-w-0 space-y-0.5">
        <p className="flex flex-wrap items-center gap-2">
          <Pill tone={delivery.status}>{tOr(`webhooks.status.${delivery.status}`, delivery.status)}</Pill>
          <span dir="ltr" className="font-mono text-xs">
            {delivery.event}
          </span>
          {delivery.lastStatus != null && <Badge>HTTP {delivery.lastStatus}</Badge>}
        </p>
        <p className="text-subtle text-xs">
          {when(delivery.createdAt, locale)} · {t('webhooks.attempts')} {delivery.attempts}
        </p>
        {delivery.lastError && (
          <p dir="ltr" className="break-all text-danger text-xs">
            {delivery.lastError}
          </p>
        )}
      </div>
      {delivery.status === 'failed' && (
        <Button variant="secondary" disabled={retry.isPending} onClick={() => retry.mutate()}>
          {t('webhooks.retry')}
        </Button>
      )}
      <ErrorNote error={retry.error} />
    </li>
  )
}

function Deliveries({ id }: { id: string }) {
  const { t } = usePrefs()
  const deliveries = useQuery({ ...deliveriesQuery(id), refetchInterval: 10_000 })
  if (deliveries.error) return <ErrorNote error={deliveries.error} />
  if (!deliveries.data) return <p className="text-subtle text-sm">{t('common.loading')}</p>
  if (deliveries.data.length === 0) return <p className="text-muted text-sm">{t('webhooks.noDeliveries')}</p>
  return (
    <ul className="divide-y divide-line-soft">
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
    <li className="space-y-3 py-4">
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div className="min-w-0 space-y-1">
          <p className="flex flex-wrap items-center gap-2">
            <span dir="auto" className="font-medium">
              {webhook.name}
            </span>
            <Pill tone={enabled ? 'active' : 'closed'}>
              {enabled ? t('webhooks.enabled') : t('webhooks.disabled')}
            </Pill>
            {webhook.failures > 0 && (
              <Pill tone="warning">{`${t('webhooks.failures')} ${webhook.failures}`}</Pill>
            )}
          </p>
          <p dir="ltr" className="break-all text-start font-mono text-muted text-xs">
            {webhook.url}
          </p>
          <p className="flex flex-wrap gap-1">
            {webhook.events.map((e) => (
              <Badge key={e}>{e === '*' ? t('webhooks.allEvents') : e}</Badge>
            ))}
          </p>
          {webhook.disabledAt && (
            <p className="text-subtle text-xs">
              {t('webhooks.disabledAt')} {when(webhook.disabledAt, locale)}
            </p>
          )}
        </div>
        <div className="flex flex-wrap gap-2">
          <Button variant="secondary" onClick={() => setOpen((v) => !v)} aria-expanded={open}>
            {open ? t('webhooks.hideDeliveries') : t('webhooks.showDeliveries')}
          </Button>
          {enabled && (
            <Button variant="secondary" disabled={ping.isPending} onClick={() => ping.mutate()}>
              {t('webhooks.ping')}
            </Button>
          )}
        </div>
      </div>
      <ErrorNote error={ping.error} />
      {open && <Deliveries id={webhook.id} />}
      {enabled && (
        <ConfirmDelete
          name={webhook.name}
          what={t('webhooks.what')}
          pending={disable.isPending}
          error={disable.error}
          onConfirm={() => disable.mutate()}
        />
      )}
    </li>
  )
}

/** Signed webhooks of the organization. */
export function WebhooksPage() {
  const { t } = usePrefs()
  const webhooks = useQuery(webhooksQuery)
  return (
    <section className="space-y-6">
      <PageHeader title={t('webhooks.title')} subtitle={t('webhooks.lead')} />
      <CreateWebhook />
      <ErrorNote error={webhooks.error} />
      {webhooks.data && webhooks.data.length === 0 && <Empty>{t('webhooks.empty')}</Empty>}
      {webhooks.data && webhooks.data.length > 0 && (
        <Card title={t('webhooks.endpoints')}>
          <ul className="divide-y divide-line-soft">
            {webhooks.data.map((w) => (
              <WebhookRow key={w.id} webhook={w} />
            ))}
          </ul>
        </Card>
      )}
    </section>
  )
}
