import { Check } from "lucide-react";
import type { InputHTMLAttributes } from "react";

export function Checkbox({ className = "", ...props }: Omit<InputHTMLAttributes<HTMLInputElement>, "type">) {
  return <span className={`relative inline-flex size-5 shrink-0 align-middle ${className}`}>
    <input {...props} type="checkbox" className="peer absolute inset-0 z-10 m-0 size-full cursor-pointer opacity-0 disabled:cursor-not-allowed" />
    <span aria-hidden="true" className="pointer-events-none flex size-full items-center justify-center rounded border border-line-input bg-panel text-canvas transition-colors peer-hover:border-fg-muted peer-checked:border-info peer-checked:bg-info peer-focus-visible:outline-2 peer-focus-visible:outline-offset-2 peer-focus-visible:outline-info peer-disabled:opacity-40 peer-[:checked]:[&>svg]:opacity-100">
      <Check className="size-3.5 opacity-0" strokeWidth={3} />
    </span>
  </span>;
}
