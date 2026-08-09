import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Badge, Button, Card, ErrorNote, PageHeader, Select, TextField } from '../components/ui'
import { inviteMember, membersQuery, removeMember, updateMember } from '../lib/api'

const route = getRouteApi('/_authed')
const ROLES = ['viewer', 'developer', 'admin', 'owner'] as const

export function TeamPage() {
  const { me } = route.useRouteContext()
  const { data: members } = useSuspenseQuery(membersQuery)
