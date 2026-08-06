import type { components } from '@kuben/api-client'
import { api } from '@kuben/api-client'
import { queryOptions } from '@tanstack/react-query'
import { toApiError } from './problem'

type Schemas = components['schemas']
export type User = Schemas['UserDto']
export type Project = Schemas['ProjectDto']
export type Environment = Schemas['EnvironmentDto']
export type EnvType = Schemas['EnvType']
export type App = Schemas['AppDto']
export type AppDetail = Schemas['AppDetail']
export type Pod = Schemas['PodDto']
export type PodLogs = Schemas['PodLogs']
export type Secret = Schemas['SecretDto']
export type EnvVar = Schemas['EnvVarDto']
export type Volume = Schemas['VolumeDto']
export type CreateApp = Schemas['CreateApp']
export type UpdateApp = Schemas['UpdateApp']
export type Release = Schemas['ReleaseDto']
export type DomainCheck = Schemas['DomainCheck']
export type PromoteResult = Schemas['PromoteResult']
export type Template = Schemas['TemplateDto']
export type DeployedTemplate = Schemas['DeployedTemplate']
export type Token = Schemas['TokenDto']
export type CreateToken = Schemas['CreateToken']
export type CreatedToken = Schemas['CreatedToken']
export type Member = Schemas['MemberDto']
export type InvitedMember = Schemas['InvitedMember']
export type AuditEvent = Schemas['AuditEventDto']
export type AuditPage = Schemas['AuditPage']

interface Outcome<T> {
  data?: T
  error?: unknown
  response: Response
}

/** Resolve an openapi-fetch call to its data, or throw an `ApiError`. */
async function unwrap<T>(request: Promise<Outcome<T>>): Promise<T> {
