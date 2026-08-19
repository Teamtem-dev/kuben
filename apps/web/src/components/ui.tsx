import {
  type ButtonHTMLAttributes,
  type InputHTMLAttributes,
  type ReactNode,
  type SelectHTMLAttributes,
  type TextareaHTMLAttributes,
  useId,
  useState,
} from 'react'
import { problemMessage } from '../lib/problem'

const buttonVariants = {
  primary: 'bg-sky-500 text-slate-950 hover:bg-sky-400',
  secondary: 'border border-white/10 hover:bg-white/5',
  danger: 'bg-red-500/90 text-white hover:bg-red-500',
  ghost: 'text-slate-400 hover:bg-white/5 hover:text-slate-100',
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

const control =
  'w-full rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm outline-none transition placeholder:text-slate-600 focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30'

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
      {hint && <p className="text-slate-500 text-xs">{hint}</p>}
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
      {hint && <p className="text-slate-500 text-xs">{hint}</p>}
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
      {hint && <p className="text-slate-500 text-xs">{hint}</p>}
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
    <section className="rounded-xl border border-white/10 bg-slate-900/50">
      {(title || actions) && (
        <header className="flex items-center justify-between gap-3 border-white/10 border-b px-4 py-3">
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
      {crumbs && <nav className="flex flex-wrap items-center gap-1.5 text-slate-500 text-sm">{crumbs}</nav>}
      <div className="flex flex-wrap items-end justify-between gap-4">
        <div className="min-w-0">
          <h1 className="truncate font-semibold text-xl">{title}</h1>
          {subtitle && <p className="text-slate-400 text-sm">{subtitle}</p>}
        </div>
        {actions && <div className="flex flex-wrap gap-2">{actions}</div>}
      </div>
    </header>
  )
}

export function Status({ ready, label }: { ready: boolean; label?: string | null }) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 font-medium text-xs ${ready ? 'bg-emerald-400/10 text-emerald-300' : 'bg-amber-400/10 text-amber-300'}`}
    >
      <span className={`size-1.5 rounded-full ${ready ? 'bg-emerald-400' : 'bg-amber-400'}`} />
      {label ?? (ready ? 'Ready' : 'Not ready')}
    </span>
  )
}

export function Badge({ children }: { children: ReactNode }) {
  return (
    <span className="rounded-md bg-white/5 px-1.5 py-0.5 font-mono text-slate-300 text-xs">{children}</span>
  )
}

export function Empty({ children }: { children: ReactNode }) {
  return (
    <div className="rounded-xl border border-white/15 border-dashed p-10 text-center text-slate-400 text-sm">
      {children}
    </div>
  )
}

export function ErrorNote({ error }: { error: unknown }) {
  if (!error) return null
  return (
    <p role="alert" className="rounded-lg bg-red-500/10 px-3 py-2 text-red-300 text-sm">
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
