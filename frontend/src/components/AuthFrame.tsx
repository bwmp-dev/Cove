import type { ReactNode } from "react";

export function AuthFrame({ title, description, children }: { title: string; description?: ReactNode; children: ReactNode }) {
  return (
    <main className="flex min-h-dvh items-start justify-center bg-canvas px-4 pt-[12vh] pb-12">
      <div className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2.5">
          <svg aria-hidden viewBox="0 0 32 32" className="size-7">
            <rect width="32" height="32" rx="7" className="fill-raised" />
            <path d="M21.5 11.2A7 7 0 1 0 21.5 20.8" fill="none" className="stroke-fg" strokeWidth="3" strokeLinecap="round" />
          </svg>
          <span className="text-[15px] font-semibold tracking-tight">Cove</span>
        </div>
        <div className="rounded-lg border border-line bg-panel p-5">
          <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
          {description && <div className="mt-1 text-[13px] text-fg-muted">{description}</div>}
          <div className="mt-5">{children}</div>
        </div>
      </div>
    </main>
  );
}
