import type { KeyboardEvent } from "react";
import { describedBy, inputClass } from "../lib/styles";

const ranges = {
  cpus: { inputMode: "decimal", min: "0.01", max: "1024", step: "0.01" },
  memory: { inputMode: "numeric", min: "6", max: "16777216", step: "1" },
} as const;

// A CPU-cores or memory-MiB input wired to its Field's hint and error. Blank means no value.
export function LimitInput({ id, kind, value, error, onChange, onKeyDown }: { id: string; kind: keyof typeof ranges; value: string; error?: string; onChange: (value: string) => void; onKeyDown?: (event: KeyboardEvent<HTMLInputElement>) => void }) {
  return (
    <input
      id={id}
      className={inputClass}
      type="number"
      {...ranges[kind]}
      value={value}
      onChange={(event) => onChange(event.target.value)}
      onKeyDown={onKeyDown}
      aria-invalid={error ? true : undefined}
      aria-describedby={describedBy(id, error, true)}
    />
  );
}
