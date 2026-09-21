import {
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
  useId,
  useState,
} from 'react'
import { fill } from '../lib/messages/pages'
import { usePrefs } from '../lib/prefs'
import { problemMessage } from '../lib/problem'
import { Logo } from './brand'
import { Preferences } from './preferences'

const buttonVariants = {
  primary: 'bg-brand text-on-brand hover:bg-brand-hover',
  secondary: 'border border-line hover:bg-hover',
  danger: 'bg-danger-solid/90 text-white hover:bg-danger-solid',
  ghost: 'text-muted-foreground hover:bg-hover hover:text-fg',
} as const

type ButtonProps = ButtonHTMLAttributes<HTMLButtonElement> & { variant?: keyof typeof buttonVariants }

export function Button({ variant = 'primary', className = '', ...props }: ButtonProps) {
  return (
    <button
      type="button"
      {...props}
      className={`inline-flex items-center justify-center gap-2 rounded-lg px-3 py-1.5 font-medium text-sm transition disabled:cursor-not-allowed disabled:opacity-50 ${buttonVariants[variant]} ${className}`}
    />
  )
}

/** The look of every text input, select and text area. */
export const control =
  'w-full rounded-lg border border-line bg-canvas px-3 py-2 text-sm outline-none transition placeholder:text-subtle focus:border-brand focus:ring-2 focus:ring-brand/30'

interface FieldProps {
  label: string
  hint?: ReactNode
}

export function TextField({ label, hint, ...props }: FieldProps & InputHTMLAttributes<HTMLInputElement>) {
  const id = useId()
  return (
    <div className="space-y-1.5">
      <label htmlFor={id} className="font-medium text-sm">
        {label}
      </label>
      <input id={id} className={control} {...props} />
      {hint && <p className="text-subtle text-xs">{hint}</p>}
    </div>
  )
}

export function TextArea({
  label,
  hint,
  ...props
}: FieldProps & TextareaHTMLAttributes<HTMLTextAreaElement>) {
  const id = useId()
  return (
    <div className="space-y-1.5">
      <label htmlFor={id} className="font-medium text-sm">
        {label}
      </label>
      <textarea id={id} className={`${control} min-h-24 font-mono`} spellCheck={false} {...props} />
      {hint && <p className="text-subtle text-xs">{hint}</p>}
    </div>
  )
}

export function Select({
  label,
  hint,
  children,
  ...props
}: FieldProps & SelectHTMLAttributes<HTMLSelectElement> & { children: ReactNode }) {
  const id = useId()
  return (
    <div className="space-y-1.5">
      <label htmlFor={id} className="font-medium text-sm">
        {label}
      </label>
      <select id={id} className={control} {...props}>
        {children}
      </select>
      {hint && <p className="text-subtle text-xs">{hint}</p>}
    </div>
  )
}

export function Card({
  title,
  actions,
  children,
}: {
  title?: ReactNode
  actions?: ReactNode
  children: ReactNode
}) {
  return (
    <section className="rounded-xl border border-line bg-surface">
      {(title || actions) && (
        <header className="flex items-center justify-between gap-3 border-line border-b px-4 py-3">
          <h2 className="font-medium text-sm">{title}</h2>
          {actions}
        </header>
      )}
      <div className="p-4">{children}</div>
    </section>
  )
}

export function PageHeader({
  title,
  subtitle,
  actions,
  crumbs,
}: {
  title: ReactNode
  subtitle?: ReactNode
  actions?: ReactNode
  crumbs?: ReactNode
}) {
  return (
    <header className="space-y-2">
      {crumbs && <nav className="flex flex-wrap items-center gap-1.5 text-subtle text-sm">{crumbs}</nav>}
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="min-w-0">
          <h1 className="truncate font-semibold text-xl">{title}</h1>
          {subtitle && <p className="text-muted-foreground text-sm">{subtitle}</p>}
        </div>
        {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
      </div>
    </header>
  )
}

export function Status({ ready, label }: { ready: boolean; label?: string | null }) {
  const { t } = usePrefs()
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-medium text-xs ${ready ? 'bg-ok/10 text-ok' : 'bg-warn/10 text-warn'}`}
    >
      <span className={`size-1.5 rounded-full ${ready ? 'bg-ok' : 'bg-warn'}`} />
      {label ?? (ready ? t('ui.ready') : t('ui.notReady'))}
    </span>
  )
}

export function Badge({ children }: { children: ReactNode }) {
  return <span className="rounded-md bg-hover px-1.5 py-0.5 font-mono text-fg-soft text-xs">{children}</span>
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-xl border border-line-strong border-dashed p-10 text-center text-muted-foreground text-sm">
      {children}
    </div>
  )
}

export function ErrorNote({ error }: { error: unknown }) {
  if (!error) return null
  return (
    <p role="alert" className="rounded-lg bg-danger/10 px-3 py-2 text-danger text-sm">
      {problemMessage(error)}
    </p>
  )
}

/** Destructive action guarded by typing the resource name (GitHub-style). */
export function ConfirmDelete({
  name,
  what,
  pending,
  error,
  onConfirm,
}: {
  name: string
  what: string
  pending: boolean
  error?: unknown
  onConfirm: () => void
}) {
  const { t } = usePrefs()
  const [open, setOpen] = useState(false)
  const [typed, setTyped] = useState('')
  if (!open) {
    return (
      <Button variant="ghost" onClick={() => setOpen(true)}>
        {fill(t('ui.deleteWhat'), { what })}
      </Button>
    )
  }
  return (
    <div className="w-full space-y-3 rounded-xl border border-danger/30 bg-danger/5 p-4">
      <TextField
        label={fill(t('ui.typeToDelete'), { name, what })}
        value={typed}
        onChange={(e) => setTyped(e.target.value)}
        autoComplete="off"
      />
      <ErrorNote error={error} />
      <div className="flex gap-2">
        <Button variant="danger" disabled={typed !== name || pending} onClick={onConfirm}>
          {pending ? t('ui.deleting') : fill(t('ui.deleteWhat'), { what })}
        </Button>
        <Button variant="secondary" onClick={() => setOpen(false)}>
          {t('ui.cancel')}
        </Button>
      </div>
    </div>
  )
}

/** The pages before signing in: the mark and the preferences above a card. */
export function AuthLayout({ children }: { children: ReactNode }) {
  const { t } = usePrefs()
  return (
    <main className="flex min-h-dvh flex-col items-center gap-8 p-6">
      <div className="flex w-full max-w-lg items-center justify-between">
        <Logo label={t('app.name')} />
        <Preferences />
      </div>
      <div className="grid w-full flex-1 place-items-center">{children}</div>
    </main>
  )
}
