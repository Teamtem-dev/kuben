/**
 * Page building blocks on the shadcn/ui components: headers, sections,
 * labelled inputs, notices and the guarded delete. Pages compose these
 * instead of styling the primitives each time.
 */
import {
  AlertCircleIcon,
  CheckCircle2Icon,
  CheckIcon,
  CopyIcon,
  InfoIcon,
  TriangleAlertIcon,
} from 'lucide-react'
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
import { Checkbox } from '@/components/ui/checkbox'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from '@/components/ui/dialog'
import { Empty, EmptyContent, EmptyDescription } from '@/components/ui/empty'
import { Field, FieldDescription, FieldLabel } from '@/components/ui/field'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { NativeSelect } from '@/components/ui/native-select'
import { Switch } from '@/components/ui/switch'
import { Textarea } from '@/components/ui/textarea'
import { fill } from '@/lib/messages/pages'
import { sparkline, stateTones, type Tone } from '@/lib/ops'
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

const toneClasses: Record<Tone, string> = {
  success: 'border-success/30 bg-success/10 text-success',
  warning: 'border-warning/30 bg-warning/10 text-warning',
  danger: 'border-destructive/30 bg-destructive/10 text-destructive',
  neutral: 'border-border bg-muted text-muted-foreground',
}

/**
 * A state or a severity as a coloured badge: `tone` names the colour, or a
 * state (`open`, `verified`, `failed`, …) whose colour `lib/ops` knows.
 */
export function ToneBadge({
  tone,
  children,
  className,
}: {
  tone: Tone | string
  children: ReactNode
  className?: string
}) {
  const family: Tone = tone in toneClasses ? (tone as Tone) : (stateTones[tone] ?? 'neutral')
  return (
    <Badge variant="outline" className={cn(toneClasses[family], className)}>
      {children}
    </Badge>
  )
}

/** A line of muted text while something loads. */
export function Loading({ children, className }: { children?: ReactNode; className?: string }) {
  const { t } = usePrefs()
  return (
    <p role="status" className={cn('text-muted-foreground text-sm', className)}>
      {children ?? t('common.loading')}
    </p>
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

/** A checkbox and its label; with `name` it takes part in its form (`on` when checked). */
export function CheckboxField({
  label,
  className,
  ...props
}: { label: ReactNode; className?: string } & ComponentProps<typeof Checkbox>) {
  const id = useId()
  return (
    <div className={cn('flex items-center gap-2', className)}>
      <Checkbox id={id} {...props} />
      <Label htmlFor={id} className="font-normal">
        {label}
      </Label>
    </div>
  )
}

/** An on/off switch and its label, for a setting or a filter that applies at once. */
export function SwitchField({
  label,
  className,
  ...props
}: { label: ReactNode; className?: string } & ComponentProps<typeof Switch>) {
  const id = useId()
  return (
    <div className={cn('flex items-center gap-2', className)}>
      <Switch id={id} {...props} />
      <Label htmlFor={id} className="font-normal">
        {label}
      </Label>
    </div>
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

/**
 * An action that removes or stops something, confirmed in a dialog first
 * (no name to type: for things that are easy to make again). The dialog
 * stays open, with the error, when `onConfirm` fails.
 */
export function ConfirmAction({
  label,
  title,
  description,
  pending,
  error,
  onConfirm,
  variant = 'destructive',
  size,
  disabled,
}: {
  label: ReactNode
  title: ReactNode
  description: ReactNode
  pending: boolean
  error?: unknown
  onConfirm: () => Promise<unknown>
  variant?: 'destructive' | 'outline' | 'ghost'
  size?: 'sm' | 'default'
  disabled?: boolean
}) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  return (
    <AlertDialog open={open} onOpenChange={setOpen}>
      <AlertDialogTrigger asChild>
        <Button
          variant={variant}
          size={size}
          disabled={disabled}
          className={cn(variant === 'ghost' && 'text-destructive hover:text-destructive')}
        >
          {label}
        </Button>
      </AlertDialogTrigger>
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{title}</AlertDialogTitle>
          <AlertDialogDescription>{description}</AlertDialogDescription>
        </AlertDialogHeader>
        <ErrorAlert error={error} />
        <AlertDialogFooter>
          <AlertDialogCancel>{t('ui.cancel')}</AlertDialogCancel>
          <Button
            variant="destructive"
            disabled={pending}
            onClick={() => {
              onConfirm().then(
                () => setOpen(false),
                () => undefined,
              )
            }}
          >
            {label}
          </Button>
        </AlertDialogFooter>
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

/** Copies `value` to the clipboard and says so for a moment. */
export function CopyButton({ value, className }: { value: string; className?: string }) {
  const { t } = usePrefs()
  const [copied, setCopied] = useState(false)
  return (
    <Button
      type="button"
      variant="outline"
      size="sm"
      className={className}
      onClick={() => {
        void navigator.clipboard?.writeText(value).then(() => {
          setCopied(true)
          setTimeout(() => setCopied(false), 1500)
        })
      }}
    >
      {copied ? <CheckIcon aria-hidden="true" /> : <CopyIcon aria-hidden="true" />}
      <span aria-live="polite">{copied ? t('ui.copied') : t('ui.copy')}</span>
    </Button>
  )
}

/** A form in a dialog, opened by `trigger`; the form closes it through `onOpenChange`. */
export function FormDialog({
  open,
  onOpenChange,
  trigger,
  title,
  description,
  children,
  className,
}: {
  open: boolean
  onOpenChange: (open: boolean) => void
  trigger: ReactNode
  title: ReactNode
  description?: ReactNode
  children: ReactNode
  className?: string
}) {
  return (
    <Dialog open={open} onOpenChange={onOpenChange}>
      <DialogTrigger asChild>{trigger}</DialogTrigger>
      <DialogContent showCloseButton={false} className={className}>
        <DialogHeader>
          <DialogTitle>{title}</DialogTitle>
          {description ? (
            <DialogDescription>{description}</DialogDescription>
          ) : (
            <DialogDescription className="sr-only">{title}</DialogDescription>
          )}
        </DialogHeader>
        {children}
      </DialogContent>
    </Dialog>
  )
}

/** The look of a card that is one link (a project, an environment, an app). */
export const linkCard =
  'block h-full rounded-xl border bg-card p-4 text-card-foreground shadow-xs outline-none transition hover:border-foreground/20 hover:bg-accent/40 focus-visible:ring-[3px] focus-visible:ring-ring/50'

/** A value to copy (a DNS record, a secret shown once): the text and a copy button. */
export function Copyable({ value }: { value: string }) {
  return (
    <span className="flex min-w-0 items-center gap-2">
      <code
        dir="ltr"
        className="min-w-0 select-all break-all rounded-md bg-muted px-2 py-1 text-start font-mono text-xs"
      >
        {value}
      </code>
      <CopyButton value={value} className="shrink-0" />
    </span>
  )
}

/** A line chart of `values`, labelled for screen readers. */
export function Sparkline({ values, label }: { values: readonly number[]; label: string }) {
  return (
    <svg
      viewBox="0 0 240 48"
      className="h-12 w-full"
      role="img"
      aria-label={label}
      preserveAspectRatio="none"
    >
      <polyline
        points={sparkline(values, 240, 48)}
        fill="none"
        className="stroke-primary"
        strokeWidth="2"
        vectorEffect="non-scaling-stroke"
        strokeLinejoin="round"
      />
    </svg>
  )
}
