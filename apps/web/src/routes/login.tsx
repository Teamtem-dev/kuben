import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useId } from 'react'
import { login, meQuery } from '../lib/api'
import { problemMessage } from '../lib/problem'

const route = getRouteApi('/login')

const inputClass =
  'w-full rounded-lg border border-white/10 bg-slate-950 px-3 py-2 text-sm outline-none transition focus:border-sky-400 focus:ring-2 focus:ring-sky-400/30'

export function LoginPage() {
  const { redirect } = route.useSearch()
  const router = useRouter()
  const queryClient = useQueryClient()
  const emailId = useId()
  const passwordId = useId()

  const mutation = useMutation({
    mutationFn: (creds: { email: string; password: string }) => login(creds.email, creds.password),
    onSuccess: (user) => {
      queryClient.setQueryData(meQuery.queryKey, user)
      router.history.push(redirect ?? '/')
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const form = new FormData(event.currentTarget)
    mutation.mutate({ email: String(form.get('email') ?? ''), password: String(form.get('password') ?? '') })
  }

  return (
