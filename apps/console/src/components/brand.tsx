/** Kuben's mark (the favicon's) and its name. */
export function Logo({ label }: { label: string }) {
  return (
    <span className="inline-flex items-center gap-2 font-semibold tracking-tight">
      <svg viewBox="0 0 32 32" className="size-7 shrink-0" aria-hidden="true">
        <rect width="32" height="32" rx="7" className="fill-primary" />
        <path
          d="M9 7v18M9 16l9-9M12.5 12.5 22 25"
          fill="none"
          className="stroke-primary-foreground"
          strokeWidth="3.2"
          strokeLinecap="round"
          strokeLinejoin="round"
        />
      </svg>
      <span>{label}</span>
    </span>
  )
}

const icons = {
  projects: 'M4 6h6v6H4zM14 6h6v6h-6zM4 16h6v4H4zM14 16h6v4h-6z',
  team: 'M9 11a3 3 0 1 0 0-6 3 3 0 0 0 0 6zM3 20a6 6 0 0 1 12 0M16 11a3 3 0 1 0 0-6M21 20a6 6 0 0 0-4-5.6',
  tokens: 'M14 10a4 4 0 1 0-3.9 4.9L8 17v3h3v-2h2l1.1-1.1A4 4 0 0 0 14 10z',
  audit: 'M6 4h9l3 3v13H6zM9 11h6M9 15h6',
  incidents: 'M12 4 3 20h18zM12 10v4M12 17h.01',
  webhooks:
    'M10 14a4 4 0 0 1 0-5.7l2-2a4 4 0 0 1 5.7 5.7l-1 1M14 10a4 4 0 0 1 0 5.7l-2 2a4 4 0 0 1-5.7-5.7l1-1',
  domains:
    'M12 3a9 9 0 1 0 0 18 9 9 0 0 0 0-18zM3 12h18M12 3c2.5 2.5 3.8 5.5 3.8 9s-1.3 6.5-3.8 9c-2.5-2.5-3.8-5.5-3.8-9S9.5 5.5 12 3z',
  menu: 'M4 7h16M4 12h16M4 17h16',
  close: 'M6 6l12 12M18 6 6 18',
} as const

export function Icon({ name }: { name: keyof typeof icons }) {
  return (
    <svg
      viewBox="0 0 24 24"
      className="size-4 shrink-0"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.8"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d={icons[name]} />
    </svg>
  )
}
