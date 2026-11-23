import { useQuery } from "@tanstack/react-query";
import { useId, useState, type FormEvent } from "react";
import { nodeCostQuery, useUpdateNodeCost, type NodeCost as Cost } from "../api/advice";
import { errorMessage } from "../api/client";
import { currencyDigits, formatMinor, minorToInput, parseAmount } from "../lib/costs";
import { useOnline } from "../lib/hooks";
import { describedBy, inputClass } from "../lib/styles";
import { Button, Field, LoadingRows, Notice, QueryError, Section } from "./ui";

function CostForm({ nodeId, saved }: { nodeId: string; saved: Cost }) {
  const id = useId();
  const online = useOnline();
  const update = useUpdateNodeCost(nodeId);
  const [currency, setCurrency] = useState(saved.configured ? saved.currency : "");
  const [amount, setAmount] = useState(saved.configured ? minorToInput(saved.monthly_minor, saved.currency) : "");
  const [errors, setErrors] = useState<{ currency?: string; amount?: string }>({});
  const code = currency.trim().toUpperCase();
  const parsed = parseAmount(amount, code);
  const dirty = !saved.configured || code !== saved.currency || !("minor" in parsed) || parsed.minor !== saved.monthly_minor;

  const submit = (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault();
    if ("error" in parsed) {
      setErrors(currencyDigits(code) === undefined ? { currency: parsed.error } : { amount: parsed.error });
      return;
    }
    setErrors({});
    update.mutate({ monthly_minor: parsed.minor, currency: code });
  };

  return (
    <form onSubmit={submit} className="space-y-4" noValidate>
      <div className="grid gap-4 sm:grid-cols-[8rem_minmax(0,1fr)]">
        <Field id={`${id}-currency`} label="Currency" hint="ISO 4217 code" error={errors.currency}>
          <input
            id={`${id}-currency`}
            className={`${inputClass} font-mono uppercase`}
            value={currency}
            onChange={(event) => {
              update.reset();
              setCurrency(event.target.value);
            }}
            maxLength={3}
            placeholder="USD"
            autoComplete="off"
            spellCheck={false}
            aria-describedby={describedBy(`${id}-currency`, errors.currency, true)}
            aria-invalid={errors.currency ? true : undefined}
            required
          />
        </Field>
        <Field id={`${id}-amount`} label="Monthly price" hint="0 records the node as free. Prices are never converted between currencies." error={errors.amount}>
          <input
            id={`${id}-amount`}
            className={`${inputClass} tabular-nums`}
            value={amount}
            onChange={(event) => {
              update.reset();
              setAmount(event.target.value);
            }}
            inputMode="decimal"
            placeholder="0.00"
            autoComplete="off"
            aria-describedby={describedBy(`${id}-amount`, errors.amount, true)}
            aria-invalid={errors.amount ? true : undefined}
            required
          />
        </Field>
      </div>
      {update.isError && (
        <Notice tone="error" title="Could not save the price">
          {errorMessage(update.error)}
        </Notice>
      )}
      {update.isSuccess && !dirty && <Notice tone="success" title="Price saved" />}
      <div className="flex justify-end">
        <Button type="submit" variant="primary" disabled={!online || !dirty} pending={update.isPending}>
          Save price
        </Button>
      </div>
    </form>
  );
}

// Instance administrators record what a node costs each month so projects can see an estimated share.
export function NodeCost({ nodeId }: { nodeId: string }) {
  const cost = useQuery(nodeCostQuery(nodeId));
  return (
    <Section
      title="Node price"
      description="What this node costs per month. Project cost estimates split it equally across the resources configured on the node. One current price is kept; there is no billing history."
    >
      {cost.isPending ? (
        <LoadingRows label="Loading price" rows={1} />
      ) : cost.isError ? (
        <QueryError what="the node price" error={cost.error} onRetry={() => void cost.refetch()} retrying={cost.isFetching} />
      ) : (
        <div className="space-y-4">
          <p className="text-[13px] text-fg">
            {cost.data.configured ? (
              <>
                Current price: <span className="font-medium tabular-nums">{formatMinor(cost.data.monthly_minor, cost.data.currency)}</span> per month
                {cost.data.monthly_minor === 0 && <span className="text-fg-muted"> (free)</span>}
              </>
            ) : (
              <span className="text-fg-muted">No price set. Project estimates show this node&apos;s cost as unknown, not free.</span>
            )}
          </p>
          <CostForm nodeId={nodeId} saved={cost.data} />
        </div>
      )}
    </Section>
  );
}
