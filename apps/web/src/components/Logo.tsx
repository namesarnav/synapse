// Brand mark: rust rounded square with a small node graph.
export function Logo({ className = "logo-mark" }: { className?: string }) {
  return (
    <svg className={className} viewBox="0 0 32 32" aria-hidden="true" focusable="false">
      <rect width="32" height="32" rx="8" fill="#b8501c" />
      <path d="M9 21l7-10 7 10" fill="none" stroke="#f3dccb" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
      <circle cx="9" cy="21" r="3.2" fill="#fff" />
      <circle cx="16" cy="11" r="3.2" fill="#fff" />
      <circle cx="23" cy="21" r="3.2" fill="#f3b48c" />
    </svg>
  );
}
