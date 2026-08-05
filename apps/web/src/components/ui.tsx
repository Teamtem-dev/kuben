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
