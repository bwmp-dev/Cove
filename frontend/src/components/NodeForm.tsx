import { useState, type FormEvent, type ReactNode } from "react";
import type { NodeInput } from "../api/types";
import { validateNode, type NodeErrors as Errors } from "../lib/nodes";
import { describedBy, inputClass } from "../lib/styles";
import { Field } from "./ui";

interface NodeFormProps {
  initial?: NodeInput;
  disabled?: boolean;
  onSubmit: (input: NodeInput) => void;
  children: ReactNode;
}

export function NodeForm({ initial, disabled, onSubmit, children }: NodeFormProps) {
  const [name, setName] = useState(initial?.name ?? "");
  const [address, setAddress] = useState(initial?.address ?? "");
  const [port, setPort] = useState(String(initial?.port ?? 22));
  const [errors, setErrors] = useState<Errors>({});

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    const result = validateNode({ name, address, port });
    setErrors(result.errors);
    if (result.input) onSubmit(result.input);
  }

  const addressHint = "Must be reachable from the main node, for example through a private network or VPN.";

  return (
    <form onSubmit={submit} noValidate className="max-w-xl space-y-5">
      <fieldset disabled={disabled} className="space-y-5">
        <Field id="node-name" label="Name" error={errors.name} hint="Shown in Cove only.">
          <input
            id="node-name"
            className={inputClass}
            value={name}
            onChange={(event) => setName(event.target.value)}
            maxLength={120}
            autoComplete="off"
            aria-invalid={errors.name ? true : undefined}
            aria-describedby={describedBy("node-name", errors.name, true)}
          />
        </Field>
        <div className="grid gap-5 sm:grid-cols-[1fr_7rem]">
          <Field id="node-address" label="SSH address" error={errors.address} hint={addressHint}>
            <input
              id="node-address"
              className={`${inputClass} font-mono`}
              value={address}
              onChange={(event) => setAddress(event.target.value)}
              placeholder="10.0.0.12"
              autoComplete="off"
              autoCapitalize="none"
              spellCheck={false}
              aria-invalid={errors.address ? true : undefined}
              aria-describedby={describedBy("node-address", errors.address, addressHint)}
            />
          </Field>
          <Field id="node-port" label="SSH port" error={errors.port}>
            <input
              id="node-port"
              className={`${inputClass} font-mono`}
              value={port}
              onChange={(event) => setPort(event.target.value)}
              inputMode="numeric"
              autoComplete="off"
              aria-invalid={errors.port ? true : undefined}
              aria-describedby={describedBy("node-port", errors.port)}
            />
          </Field>
        </div>
      </fieldset>
      {children}
    </form>
  );
}
