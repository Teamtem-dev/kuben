import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useId } from 'react'
import { login, meQuery } from '../lib/api'
import { problemMessage } from '../lib/problem'

const route = getRouteApi('/login')

const inputClass =
  'w-full rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm outline-none transition focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30'

