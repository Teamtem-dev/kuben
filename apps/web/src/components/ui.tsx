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
