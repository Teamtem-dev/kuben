import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Card, ErrorNote, PageHeader, TextField } from '../components/ui'
import { changePassword, meQuery } from '../lib/api'

const route = getRouteApi('/_authed')
const MIN_LENGTH = 12

export function AccountPage() {
  const { me } = route.useRouteContext()
  const queryClient = useQueryClient()
  const router = useRouter()
  const [mismatch, setMismatch] = useState(false)
  const [done, setDone] = useState(false)

  const save = useMutation({
    mutationFn: ({ current, next }: { current: string; next: string }) => changePassword(current, next),
    onSuccess: async () => {
      setDone(true)
      // `ensureQueryData` in the route guard would keep serving the cached
      // user, so clear the flag in the cache before re-running the guards.
      queryClient.setQueryData(meQuery.queryKey, (m) => (m ? { ...m, must_change_password: false } : m))
      await router.invalidate()
    },
  })

  function onSubmit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault()
    const formElement = event.currentTarget
    const form = new FormData(formElement)
    const next = String(form.get('next') ?? '')
    const repeat = String(form.get('repeat') ?? '')
    setMismatch(next !== repeat)
    if (next !== repeat) return
    save.mutate(
      { current: String(form.get('current') ?? ''), next },
      { onSuccess: () => formElement.reset() },
    )
  }

  return (
    <section className="max-w-xl space-y-6">
