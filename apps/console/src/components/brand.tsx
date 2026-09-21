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
