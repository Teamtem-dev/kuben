import { useMutation, useQuery, useQueryClient } from '@tanstack/react-query'
import { getRouteApi, Link, Outlet, useNavigate, useRouter, useRouterState } from '@tanstack/react-router'
import {
  ChevronRightIcon,
  FolderKanbanIcon,
  GlobeIcon,
  KeyRoundIcon,
  LogOutIcon,
  type LucideIcon,
  ScrollTextIcon,
  SearchIcon,
  SirenIcon,
  UserRoundIcon,
  UsersIcon,
  WebhookIcon,
} from 'lucide-react'
import { Fragment, useEffect, useState } from 'react'
import { Logo } from '@/components/brand'
import { LanguageMenu, ThemeMenu } from '@/components/pref-menus'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  Breadcrumb,
  BreadcrumbItem,
  BreadcrumbLink,
  BreadcrumbList,
  BreadcrumbPage,
  BreadcrumbSeparator,
} from '@/components/ui/breadcrumb'
import { Button } from '@/components/ui/button'
import {
  CommandDialog,
  CommandEmpty,
  CommandGroup,
  CommandInput,
  CommandItem,
  CommandList,
} from '@/components/ui/command'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuLabel,
  DropdownMenuSeparator,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Kbd } from '@/components/ui/kbd'
import { Separator } from '@/components/ui/separator'
import {
  Sidebar,
  SidebarContent,
  SidebarGroup,
  SidebarGroupContent,
  SidebarHeader,
  SidebarMenu,
  SidebarMenuButton,
  SidebarMenuItem,
  SidebarProvider,
  SidebarRail,
  SidebarTrigger,
  useSidebar,
} from '@/components/ui/sidebar'
import { logout, projectsQuery } from '@/lib/api'
import { type Crumb, crumbsFor } from '@/lib/breadcrumbs'
import { initials } from '@/lib/initials'
import { useLiveUpdates } from '@/lib/live'
import type { MessageKey } from '@/lib/messages'
import { usePrefs } from '@/lib/prefs'

const route = getRouteApi('/_authed')

const NAV = [
  { to: '/', label: 'nav.projects', icon: FolderKanbanIcon, exact: true },
  { to: '/team', label: 'nav.team', icon: UsersIcon, exact: false },
  { to: '/tokens', label: 'nav.tokens', icon: KeyRoundIcon, exact: false },
  { to: '/incidents', label: 'nav.incidents', icon: SirenIcon, exact: false },
  { to: '/webhooks', label: 'nav.webhooks', icon: WebhookIcon, exact: false },
  { to: '/domains', label: 'nav.domains', icon: GlobeIcon, exact: false },
  { to: '/audit', label: 'nav.audit', icon: ScrollTextIcon, exact: false },
] as const satisfies readonly { to: string; label: MessageKey; icon: LucideIcon; exact: boolean }[]

/** The sidebar remembers being collapsed (shadcn keeps it in a cookie). */
const sidebarOpenAtStart = () => !document.cookie.split('; ').includes('sidebar_state=false')

const isApple = () => /Mac|iPhone|iPad/.test(navigator.userAgent)

function AppSidebar() {
  const { t } = usePrefs()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const { setOpenMobile } = useSidebar()

  // A page opened from the phone drawer closes it.
  useEffect(() => setOpenMobile(false), [pathname, setOpenMobile])

  return (
    <Sidebar collapsible="icon">
      <SidebarHeader>
        <SidebarMenu>
          <SidebarMenuItem>
            <SidebarMenuButton size="lg" asChild>
              <Link to="/">
                <Logo label={t('app.name')} />
              </Link>
            </SidebarMenuButton>
          </SidebarMenuItem>
        </SidebarMenu>
      </SidebarHeader>
      <SidebarContent>
        <SidebarGroup>
          <SidebarGroupContent>
            <nav aria-label={t('nav.label')}>
              <SidebarMenu>
                {NAV.map((item) => (
                  <SidebarMenuItem key={item.to}>
                    <SidebarMenuButton asChild tooltip={t(item.label)}>
                      <Link
                        to={item.to}
                        activeOptions={{ exact: item.exact }}
                        activeProps={{ 'data-active': true, 'aria-current': 'page' }}
                      >
                        <item.icon aria-hidden="true" />
                        <span>{t(item.label)}</span>
                      </Link>
                    </SidebarMenuButton>
                  </SidebarMenuItem>
                ))}
              </SidebarMenu>
            </nav>
          </SidebarGroupContent>
        </SidebarGroup>
      </SidebarContent>
      <SidebarRail aria-label={t('shell.toggleSidebar')} title={t('shell.toggleSidebar')} />
    </Sidebar>
  )
}

/**
 * Router links are "active" (and get aria-current="page") on every path below
 * theirs; a crumb is an ancestor of the current page, never the page itself,
 * so it is active only on an exact match — which the trail never renders as a
 * link. The current page is the last crumb, a BreadcrumbPage.
 */
const crumbActive = { exact: true } as const

/** A link to an earlier step of the trail. */
function CrumbLink({ crumb }: { crumb: Crumb }) {
  const { t } = usePrefs()
  const link = (() => {
    switch (crumb.kind) {
      case 'page':
        return (
          <Link to="/" activeOptions={crumbActive}>
            {t(crumb.label)}
          </Link>
        )
      case 'project':
        return (
          <Link to="/projects/$project" params={{ project: crumb.project }} activeOptions={crumbActive}>
            {crumb.project}
          </Link>
        )
      case 'environment':
        return (
          <Link
            to="/projects/$project/$environment"
            params={{ project: crumb.project, environment: crumb.environment }}
            activeOptions={crumbActive}
          >
            {crumb.environment}
          </Link>
        )
      case 'app':
        return (
          <Link
            to="/projects/$project/$environment/$app"
            params={{ project: crumb.project, environment: crumb.environment, app: crumb.app }}
            activeOptions={crumbActive}
          >
            {crumb.app}
          </Link>
        )
    }
  })()
  return <BreadcrumbLink asChild>{link}</BreadcrumbLink>
}

function crumbText(crumb: Crumb, t: (key: MessageKey) => string): string {
  switch (crumb.kind) {
    case 'page':
      return t(crumb.label)
    case 'project':
      return crumb.project
    case 'environment':
      return crumb.environment
    case 'app':
      return crumb.app
  }
}

function Breadcrumbs() {
  const { t } = usePrefs()
  const pathname = useRouterState({ select: (s) => s.location.pathname })
  const crumbs = crumbsFor(pathname)
  if (crumbs.length === 0) return null
  return (
    <Breadcrumb aria-label={t('shell.breadcrumb')} className="min-w-0">
      <BreadcrumbList className="flex-nowrap">
        {crumbs.map((crumb, i) => {
          const last = i === crumbs.length - 1
          return (
            <Fragment key={i}>
              {i > 0 && (
                <BreadcrumbSeparator className="hidden md:block">
                  <ChevronRightIcon className="rtl:rotate-180" />
                </BreadcrumbSeparator>
              )}
              <BreadcrumbItem className={last ? 'min-w-0' : 'hidden md:inline-flex'}>
                {last || (crumb.kind === 'page' && !crumb.to) ? (
                  <BreadcrumbPage className="truncate">{crumbText(crumb, t)}</BreadcrumbPage>
                ) : (
                  <CrumbLink crumb={crumb} />
                )}
              </BreadcrumbItem>
            </Fragment>
          )
        })}
      </BreadcrumbList>
    </Breadcrumb>
  )
}

/** ⌘K / Ctrl+K: jump to a page or a project. */
function CommandPalette() {
  const { t } = usePrefs()
  const navigate = useNavigate()
  const [open, setOpen] = useState(false)
  const projects = useQuery({ ...projectsQuery, enabled: open })

  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (event.key.toLowerCase() === 'k' && (event.metaKey || event.ctrlKey)) {
        event.preventDefault()
        setOpen((o) => !o)
      }
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [])

  const go = (to: () => Promise<void>) => {
    setOpen(false)
    void to()
  }

  return (
    <>
      <Button
        variant="outline"
        size="sm"
        className="text-muted-foreground md:w-56 md:justify-between"
        onClick={() => setOpen(true)}
        aria-keyshortcuts={isApple() ? 'Meta+K' : 'Control+K'}
      >
        <span className="inline-flex items-center gap-2">
          <SearchIcon aria-hidden="true" />
          <span className="sr-only md:not-sr-only">{t('shell.search')}</span>
        </span>
        <Kbd className="hidden md:inline-flex" aria-hidden="true">
          {isApple() ? '⌘K' : 'Ctrl K'}
        </Kbd>
      </Button>
      <CommandDialog
        open={open}
        onOpenChange={setOpen}
        title={t('command.title')}
        description={t('command.description')}
        showCloseButton={false}
      >
        <CommandInput placeholder={t('command.placeholder')} />
        <CommandList>
          <CommandEmpty>{t('command.empty')}</CommandEmpty>
          <CommandGroup heading={t('command.pages')}>
            {NAV.map((item) => (
              <CommandItem
                key={item.to}
                value={`${t(item.label)} ${item.to}`}
                onSelect={() => go(() => navigate({ to: item.to }))}
              >
                <item.icon aria-hidden="true" />
                {t(item.label)}
              </CommandItem>
            ))}
            <CommandItem
              value={`${t('shell.account')} /account`}
              onSelect={() => go(() => navigate({ to: '/account' }))}
            >
              <UserRoundIcon aria-hidden="true" />
              {t('shell.account')}
            </CommandItem>
          </CommandGroup>
          {projects.data && projects.data.length > 0 && (
            <CommandGroup heading={t('command.projects')}>
              {projects.data.map((p) => (
                <CommandItem
                  key={p.name}
                  value={`${p.display_name} ${p.name}`}
                  onSelect={() =>
                    go(() => navigate({ to: '/projects/$project', params: { project: p.name } }))
                  }
                >
                  <FolderKanbanIcon aria-hidden="true" />
                  <span className="truncate">{p.display_name}</span>
                  <span className="ms-auto text-muted-foreground text-xs" dir="ltr">
                    {p.name}
                  </span>
                </CommandItem>
              ))}
            </CommandGroup>
          )}
        </CommandList>
      </CommandDialog>
    </>
  )
}

function UserMenu() {
  const { me } = route.useRouteContext()
  const router = useRouter()
  const queryClient = useQueryClient()
  const { t } = usePrefs()
  const name = me.display_name ?? me.email

  const signOut = useMutation({
    mutationFn: logout,
    onSettled: async () => {
      queryClient.clear()
      await router.navigate({ to: '/login', search: {} })
    },
  })

  return (
    <DropdownMenu>
      <DropdownMenuTrigger asChild>
        <Button variant="ghost" className="gap-2 px-1.5" aria-label={t('shell.userMenu')}>
          <Avatar className="size-7">
            <AvatarFallback className="text-xs">{initials(name)}</AvatarFallback>
          </Avatar>
          <span className="hidden max-w-40 truncate text-sm lg:inline">{name}</span>
        </Button>
      </DropdownMenuTrigger>
      <DropdownMenuContent align="end" className="min-w-56">
        <DropdownMenuLabel className="font-normal">
          <div className="truncate font-medium text-sm">{name}</div>
          {me.display_name && (
            <div className="truncate text-muted-foreground text-xs" dir="ltr">
              {me.email}
            </div>
          )}
        </DropdownMenuLabel>
        <DropdownMenuSeparator />
        <DropdownMenuItem asChild>
          <Link to="/account">
            <UserRoundIcon aria-hidden="true" />
            {t('shell.account')}
          </Link>
        </DropdownMenuItem>
        <DropdownMenuSeparator />
        <DropdownMenuItem disabled={signOut.isPending} onSelect={() => signOut.mutate()}>
          <LogOutIcon className="rtl:-scale-x-100" aria-hidden="true" />
          {t('shell.signOut')}
        </DropdownMenuItem>
      </DropdownMenuContent>
    </DropdownMenu>
  )
}

export function AppShell() {
  const { t } = usePrefs()
  const [sidebarOpen] = useState(sidebarOpenAtStart)
  useLiveUpdates()

  return (
    <>
      <a
        href="#content"
        className="sr-only focus:not-sr-only focus:fixed focus:start-4 focus:top-4 focus:z-50 focus:rounded-md focus:bg-primary focus:px-3 focus:py-2 focus:text-primary-foreground"
      >
        {t('shell.skip')}
      </a>
      <SidebarProvider defaultOpen={sidebarOpen}>
        <AppSidebar />
        <div className="relative flex min-w-0 flex-1 flex-col bg-background">
          <header className="sticky top-0 z-10 flex h-14 shrink-0 items-center gap-2 border-b bg-background/80 px-4 backdrop-blur">
            <SidebarTrigger className="-ms-1" aria-label={t('shell.toggleSidebar')} />
            <Separator orientation="vertical" className="me-2 data-[orientation=vertical]:h-4" />
            <Breadcrumbs />
            <div className="ms-auto flex items-center gap-1">
              <CommandPalette />
              <LanguageMenu />
              <ThemeMenu />
              <UserMenu />
            </div>
          </header>
          <main
            id="content"
            tabIndex={-1}
            className="mx-auto w-full max-w-6xl flex-1 px-4 py-8 outline-none md:px-6"
          >
            <Outlet />
          </main>
        </div>
      </SidebarProvider>
    </>
  )
}
