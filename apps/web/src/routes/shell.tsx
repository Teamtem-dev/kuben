import { useMutation, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, Outlet, useRouter } from '@tanstack/react-router'
import { logout } from '../lib/api'
import { useLiveUpdates } from '../lib/live'

const route = getRouteApi('/_authed')

