import { useMutation, useQueryClient, useSuspenseQuery } from '@tanstack/react-query'
import { getRouteApi, Link, useNavigate } from '@tanstack/react-router'
import { type FormEvent, useState } from 'react'
import {
  Badge,
  Button,
  ConfirmDelete,
  Empty,
  ErrorNote,
  PageHeader,
  Select,
  Status,
  TextField,
} from '../components/ui'
import { createEnvironment, deleteProject, type EnvType, environmentsQuery, projectQuery } from '../lib/api'
