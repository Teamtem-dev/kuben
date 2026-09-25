/**
 * Page building blocks on the shadcn/ui components: headers, sections,
 * labelled inputs, notices and the guarded delete. Pages compose these
 * instead of styling the primitives each time.
 */
import { AlertCircleIcon, CheckCircle2Icon, InfoIcon, TriangleAlertIcon } from 'lucide-react'
import { type ComponentProps, type ReactNode, useId, useState } from 'react'
import { Logo } from '@/components/brand'
import { LanguageMenu, ThemeMenu } from '@/components/pref-menus'
import { Alert, AlertDescription } from '@/components/ui/alert'
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
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardAction, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Empty, EmptyContent, EmptyDescription } from '@/components/ui/empty'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { NativeSelect } from '@/components/ui/native-select'
import { Textarea } from '@/components/ui/textarea'
import { fill } from '@/lib/messages/pages'
import { usePrefs } from '@/lib/prefs'
import { problemMessage } from '@/lib/problem'
import { cn } from '@/lib/utils'

/** The page's one <h1>, a line under it, and its main actions. */
export function PageHeader({
  title,
  description,
  actions,
}: {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
}) {
  return (
    <header className="flex flex-wrap items-end justify-between gap-4">
      <div className="min-w-0 space-y-1">
        <h1 className="flex min-w-0 flex-wrap items-center gap-3 font-semibold text-2xl tracking-tight">
          {title}
        </h1>
        {description && <p className="text-muted-foreground text-sm">{description}</p>}
      </div>
      {actions && <div className="flex flex-wrap items-center gap-2">{actions}</div>}
    </header>
  )
}

/** A card with a titled header (an <h2>), for one part of a page. */
export function Section({
  title,
  description,
  actions,
  children,
  className,
  tone,
}: {
  title: ReactNode
  description?: ReactNode
  actions?: ReactNode
  children?: ReactNode
  className?: string
  tone?: 'danger'
}) {
  return (
    <Card className={cn('gap-4', tone === 'danger' && 'border-destructive/40', className)}>
      <CardHeader>
        <CardTitle>
          <h2 className={cn('text-base leading-tight', tone === 'danger' && 'text-destructive')}>{title}</h2>
        </CardTitle>
        {description && <CardDescription>{description}</CardDescription>}
        {actions && <CardAction className="flex flex-wrap items-center gap-2">{actions}</CardAction>}
      </CardHeader>
      {children && <CardContent className="space-y-4">{children}</CardContent>}
    </Card>
  )
}

/** A failed request (or any error) as an alert; nothing when there is none. */
export function ErrorAlert({ error, className }: { error: unknown; className?: string }) {
  if (!error) return null
  return (
    <Alert variant="destructive" className={className}>
      <AlertCircleIcon aria-hidden="true" />
      <AlertDescription>{problemMessage(error)}</AlertDescription>
    </Alert>
  )
}

const noticeTones = {
  info: { icon: InfoIcon, className: '' },
  success: {
    icon: CheckCircle2Icon,
    className: 'border-success/30 bg-success/5 text-success *:data-[slot=alert-description]:text-success',
  },
  warning: {
    icon: TriangleAlertIcon,
    className: 'border-warning/30 bg-warning/5 text-warning *:data-[slot=alert-description]:text-warning',
  },
} as const

/** A message about the page's state: a warning, a success, a note. */
export function Notice({
  tone = 'info',
  role = 'status',
  children,
  className,
}: {
  tone?: keyof typeof noticeTones
  role?: 'status' | 'alert'
  children: ReactNode
  className?: string
}) {
  const { icon: Icon, className: toneClass } = noticeTones[tone]
  return (
    <Alert role={role} className={cn(toneClass, className)}>
      <Icon aria-hidden="true" />
      <AlertDescription className="block">{children}</AlertDescription>
    </Alert>
  )
}

/** Ready or not, or the reason the API gives. */
export function StatusBadge({ ready, label }: { ready: boolean; label?: string | null }) {
  const { t } = usePrefs()
  return (
    <Badge
      variant="outline"
      className={
        ready
          ? 'border-success/30 bg-success/10 text-success'
          : 'border-warning/30 bg-warning/10 text-warning'
      }
    >
      <span aria-hidden="true" className={cn('size-1.5 rounded-full', ready ? 'bg-success' : 'bg-warning')} />
      {label ?? (ready ? t('ui.ready') : t('ui.notReady'))}
    </Badge>
  )
}

/** A short code-like value (a type, a role, a size). */
export function Tag({ children, className }: { children: ReactNode; className?: string }) {
  return (
    <Badge variant="secondary" className={cn('font-mono', className)}>
      {children}
    </Badge>
  )
}

/** Nothing to list yet, and what the list is for. */
export function EmptyState({ children, action }: { children: ReactNode; action?: ReactNode }) {
  return (
    <Empty className="border border-dashed">
      <EmptyDescription>{children}</EmptyDescription>
      {action && <EmptyContent>{action}</EmptyContent>}
    </Empty>
  )
}

interface Labelled {
  label: ReactNode
  hint?: ReactNode
  className?: string
}

/** A label, a control and an optional hint, wired together. */
function Labelled({
  label,
  hint,
  className,
  children,
}: Labelled & { children: (ids: { id: string; hintId?: string }) => ReactNode }) {
  const id = useId()
  const hintId = hint ? `${id}-hint` : undefined
  return (
    <Field className={cn('gap-2', className)}>
      <FieldLabel htmlFor={id}>{label}</FieldLabel>
      {children({ id, hintId })}
      {hint && <FieldDescription id={hintId}>{hint}</FieldDescription>}
    </Field>
  )
}

export function TextInput({ label, hint, className, ...props }: Labelled & ComponentProps<typeof Input>) {
  return (
    <Labelled label={label} hint={hint} className={className}>
      {({ id, hintId }) => <Input id={id} aria-describedby={hintId} {...props} />}
    </Labelled>
  )
}

export function TextareaInput({
  label,
  hint,
  className,
  ...props
}: Labelled & ComponentProps<typeof Textarea>) {
  return (
    <Labelled label={label} hint={hint} className={className}>
      {({ id, hintId }) => (
        <Textarea
          id={id}
          aria-describedby={hintId}
          className="min-h-24 font-mono"
          spellCheck={false}
          {...props}
        />
      )}
    </Labelled>
  )
}

export function SelectInput({
  label,
  hint,
  className,
  children,
  ...props
}: Labelled & ComponentProps<typeof NativeSelect>) {
  return (
    <Labelled label={label} hint={hint} className={className}>
      {({ id, hintId }) => (
        <div className="*:w-full">
          <NativeSelect id={id} aria-describedby={hintId} {...props}>
            {children}
          </NativeSelect>
        </div>
      )}
    </Labelled>
  )
}

/**
 * A destructive action guarded by typing the resource's name (GitHub-style),
 * in a confirmation dialog.
 */
export function ConfirmDelete({
  name,
  what,
  pending,
  error,
  onConfirm,
  disabled,
}: {
  name: string
  what: string
  pending: boolean
  error?: unknown
  onConfirm: () => void
  disabled?: boolean
}) {
  const { t } = usePrefs()
  const [typed, setTyped] = useState('')
  const label = fill(t('ui.deleteWhat'), { what })
  return (
    <AlertDialog onOpenChange={(open) => open || setTyped('')}>
      <AlertDialogTrigger asChild>
        <Button variant="destructive" disabled={disabled}>
          {label}
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <form
          className="grid gap-4"
          onSubmit={(event) => {
            event.preventDefault()
            if (typed === name) onConfirm()
          }}
        >
          <AlertDialogHeader>
            <AlertDialogTitle>{label}</AlertDialogTitle>
            <AlertDialogDescription>{fill(t('ui.typeToDelete'), { name, what })}</AlertDialogDescription>
          </AlertDialogHeader>
          <Input
            aria-label={fill(t('ui.typeToDelete'), { name, what })}
            value={typed}
            onChange={(e) => setTyped(e.target.value)}
            autoComplete="off"
            dir="auto"
          />
          <ErrorAlert error={error} />
          <AlertDialogFooter>
            <AlertDialogCancel type="button">{t('ui.cancel')}</AlertDialogCancel>
            <Button type="submit" variant="destructive" disabled={typed !== name || pending}>
              {pending ? t('ui.deleting') : label}
            </Button>
          </AlertDialogFooter>
        </form>
      </AlertDialogContent>
    </AlertDialog>
  )
}

/** The pages before signing in: the mark and the preferences above the content. */
export function AuthShell({ children }: { children: ReactNode }) {
  const { t } = usePrefs()
  return (
    <main className="flex min-h-dvh flex-col items-center gap-8 bg-muted/40 p-6">
      <div className="flex w-full max-w-lg items-center justify-between">
        <Logo label={t('app.name')} />
        <div className="flex items-center gap-1">
          <LanguageMenu />
          <ThemeMenu />
        </div>
      </div>
      <div className="grid w-full flex-1 place-items-center">{children}</div>
    </main>
  )
}
