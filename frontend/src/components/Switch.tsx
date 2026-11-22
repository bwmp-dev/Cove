import type { InputHTMLAttributes } from "react";

// A native checkbox announced as a switch, so forms keep reading it through checked, name and FormData.
// Use it for one on/off setting; use Checkbox when choosing several items from a set.
export function Switch({ className = "", ...props }: Omit<InputHTMLAttributes<HTMLInputElement>, "type" | "role">) {
  return <span className={`relative inline-flex h-5 w-9 shrink-0 align-middle ${className}`}>
    <input {...props} type="checkbox" role="switch" className="peer absolute inset-0 z-10 m-0 size-full cursor-pointer opacity-0 disabled:cursor-not-allowed" />
    <span aria-hidden="true" className="pointer-events-none flex size-full items-center rounded-full border border-line-input bg-panel p-0.5 transition-colors peer-hover:border-fg-muted peer-checked:border-info peer-checked:bg-info peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-info peer-disabled:opacity-40 peer-checked:[&>span]:translate-x-4 peer-checked:[&>span]:bg-white">
      <span className="size-3.5 rounded-full bg-fg-muted transition-transform" />
    </span>
  </span>;
}
