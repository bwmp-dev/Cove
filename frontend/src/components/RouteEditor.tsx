import { Plus, Trash2 } from "lucide-react";
import { useId } from "react";
import type { Route } from "../api/types";
import type { RouteRow } from "../lib/workload-form";
import { inputClass } from "../lib/styles";
import { Button } from "./ui";

export function RouteEditor({ routes, errors = {}, onChange }: { routes: RouteRow[]; errors?: Record<string, string>; onChange: (routes: RouteRow[]) => void }) {
  const id = useId();
  function update(key: string, patch: Partial<Route>) {
    onChange(routes.map((route) => (route.id === key ? { ...route, ...patch } : route)));
  }
  const label = "mb-1 block text-xs font-medium text-fg-muted";
  return (
    <div className="space-y-3">
      {routes.length > 0 && (
        <ul aria-label="Routes" className="space-y-3">
          {routes.map((route, index) => {
            const error = errors[route.id];
            const describedBy = error ? `${id}-${index}-error` : undefined;
            return (
              <li key={route.id} className="space-y-1.5">
                <div className="grid grid-cols-[minmax(0,1fr)_5.5rem_auto] items-end gap-2 sm:grid-cols-[minmax(0,2fr)_minmax(0,1fr)_6rem_auto]">
                  <div className="col-span-3 sm:col-span-1">
                    <label htmlFor={`${id}-domain-${index}`} className={label}>
                      Hostname
                    </label>
                    <input
                      id={`${id}-domain-${index}`}
                      className={`${inputClass} font-mono`}
                      value={route.domain}
                      onChange={(event) => update(route.id, { domain: event.target.value })}
                      placeholder="app.example.com"
                      inputMode="url"
                      autoComplete="off"
                      autoCapitalize="none"
                      spellCheck={false}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedBy}
                    />
                  </div>
                  <div>
                    <label htmlFor={`${id}-service-${index}`} className={label}>
                      Compose service
                    </label>
                    <input
                      id={`${id}-service-${index}`}
                      className={`${inputClass} font-mono`}
                      value={route.service}
                      onChange={(event) => update(route.id, { service: event.target.value })}
                      placeholder="web"
                      autoComplete="off"
                      autoCapitalize="none"
                      spellCheck={false}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedBy}
                    />
                  </div>
                  <div>
                    <label htmlFor={`${id}-port-${index}`} className={label}>
                      Container port
                    </label>
                    <input
                      id={`${id}-port-${index}`}
                      className={`${inputClass} font-mono tabular-nums`}
                      type="number"
                      min={1}
                      max={65535}
                      value={Number.isNaN(route.port) ? "" : route.port}
                      onChange={(event) => update(route.id, { port: event.target.valueAsNumber })}
                      aria-invalid={error ? true : undefined}
                      aria-describedby={describedBy}
                    />
                  </div>
                  <Button variant="ghost" className="px-2" aria-label={`Remove route ${route.domain || index + 1}`} title="Remove route" onClick={() => onChange(routes.filter((item) => item.id !== route.id))}>
                    <Trash2 aria-hidden className="size-4" />
                  </Button>
                </div>
                {error && (
                  <p id={describedBy} className="text-xs text-bad">
                    {error}
                  </p>
                )}
              </li>
            );
          })}
        </ul>
      )}
      <Button disabled={routes.length >= 32} onClick={() => onChange([...routes, { id: crypto.randomUUID(), domain: "", service: "", port: 80 }])}>
        <Plus aria-hidden className="size-3.5" />
        Add route
      </Button>
    </div>
  );
}
