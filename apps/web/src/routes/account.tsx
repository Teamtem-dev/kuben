import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, useRouter } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import { Button, Card, ErrorNote, PageHeader, TextField } from '../components/ui'
import { changePassword, meQuery } from '../lib/api'

const route = getRouteApi('/_authed')
const MIN_LENGTH = 12

export function AccountPage() {
