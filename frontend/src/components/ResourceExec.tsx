import { useMutation, useQuery } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import { useMemo, useState } from "react";
import { errorMessage, request } from "../api/client";
import { nodesQuery, workloadRuntimeQuery } from "../api/queries";
import type { ExecRequest, ExecResult, Workload } from "../api/types";
import { commandProblem, composeServices, containerIdPattern, execLimits, timeoutProblem } from "../lib/exec";
import { useOnline } from "../lib/hooks";
import { inputClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Button, Field, Notice } from "./ui";

interface Argument { id: string; value: string }

function newArgument(value = ""): Argument {
  return { id: crypto.randomUUID(), value };
}

function ExecOutput({ result, timeout }: { result: ExecResult; timeout: number }) {
  const succeeded = result.exit_code === 0 && !result.timed_out;
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center gap-x-4 gap-y-1 text-xs">
        {result.timed_out ? <StatusLabel tone="warn" label={`Wait timeout after ${timeout} seconds`} /> : <StatusLabel tone={succeeded ? "ok" : "bad"} label={`Exit code ${result.exit_code}`} />}
        {result.truncated && <StatusLabel tone="warn" label="Output truncated at 64 KiB" />}
      </div>
      {result.timed_out && (
        <Notice tone="warn" title="Wait timeout">
          The command may still be running in the container. Inspect it before retrying.
        </Notice>
      )}
      <pre
        role="region"
        tabIndex={0}
        aria-label="Command output"
        className="max-h-[32rem] min-h-24 overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed break-words whitespace-pre-wrap text-fg"
      >
        {result.output || <span className="text-fg-subtle">The command wrote no output.</span>}
      </pre>
    </div>
  );
}

export function ResourceExec({ workload, operationActive }: { workload: Workload; operationActive: boolean }) {
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const runtime = useQuery(workloadRuntimeQuery(workload.id, operationActive));
  const [chosenService, setChosenService] = useState("");
  const [container, setContainer] = useState("");
  const [timeout, setTimeoutSeconds] = useState(10);
  const [args, setArgs] = useState<Argument[]>(() => [newArgument()]);
  const [confirming, setConfirming] = useState(false);
  const [submitted, setSubmitted] = useState(false);

  const command = args.map((argument) => argument.value);
  const problem = commandProblem(command);
  const timeoutError = timeoutProblem(timeout);
  const node = nodes.data?.find((item) => item.id === workload.node_id);
  // Older workers do not report exec support; the control plane then refuses with an upgrade hint.
  const unsupported = node?.capabilities ? node.capabilities.exec !== true : false;
  const running = useMemo(() => (runtime.data?.containers ?? []).filter((item) => item.state === "running" && containerIdPattern.test(item.id)), [runtime.data]);
  // Git workloads save no Compose file, so the services the running containers report are the only source for them.
  const services = useMemo(() => [...new Set([...composeServices(workload.compose), ...running.flatMap((item) => (item.service ? [item.service] : []))])], [workload.compose, running]);
  const service = chosenService || (services.length === 1 ? (services[0] ?? "") : "");
  // Older workers report no service per container, so those stay listed rather than being guessed away.
  const containers = service ? running.filter((item) => !item.service || item.service === service) : running;

  function setService(value: string) {
    setChosenService(value);
    setContainer("");
  }

  const input: ExecRequest = { service, command, timeout_seconds: timeout, ...(container ? { container } : {}) };
  // The command and its output live only while this form is mounted (gcTime 0) and never enter the query cache.
  const run = useMutation({
    gcTime: 0,
    mutationFn: (body: ExecRequest) => request<ExecResult>("POST", `/api/workloads/${encodeURIComponent(workload.id)}/exec`, body),
  });

  const invalid = !service || problem !== null || timeoutError !== null;
  const blocked = !online || unsupported || run.isPending;

  function update(id: string, value: string) {
    setArgs((current) => current.map((argument) => (argument.id === id ? { ...argument, value } : argument)));
  }

  return (
    <form
      className="space-y-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        setSubmitted(true);
        if (!invalid && !blocked) setConfirming(true);
      }}
    >
      {unsupported && (
        <Notice tone="warn" title="This node's worker cannot run commands">
          Upgrade the worker on {node?.name ?? "this node"} and verify the node again before running commands.
        </Notice>
      )}
      {operationActive && <Notice title="An operation is in progress">Commands are refused until the active operation finishes.</Notice>}

      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="exec-service" label="Service" error={submitted && !service ? "Choose the Compose service to run the command in." : undefined} hint={services.length === 0 ? "Enter the Compose service name exactly." : undefined}>
          {services.length > 0 ? (
            <Select id="exec-service" value={service} aria-invalid={submitted && !service} aria-describedby={submitted && !service ? "exec-service-error" : undefined} onChange={(event) => setService(event.target.value)}>
              <option value="">Select service</option>
              {services.map((name) => (
                <option key={name} value={name}>
                  {name}
                </option>
              ))}
            </Select>
          ) : (
            <input id="exec-service" className={`${inputClass} font-mono`} value={service} autoComplete="off" spellCheck={false} aria-invalid={submitted && !service} aria-describedby={submitted && !service ? "exec-service-error" : "exec-service-hint"} onChange={(event) => setService(event.target.value.trim())} />
          )}
        </Field>
        <Field id="exec-container" label="Container" optional hint={runtime.isError ? `Running containers unavailable: ${errorMessage(runtime.error)}` : "Needed only when the service runs more than one replica."}>
          <Select id="exec-container" value={container} aria-describedby="exec-container-hint" onChange={(event) => setContainer(event.target.value)}>
            <option value="">The service's only running container</option>
            {containers.map((item) => (
              <option key={item.id} value={item.id}>
                {item.name} ({item.id.slice(0, 12)})
              </option>
            ))}
          </Select>
        </Field>
      </div>

      <fieldset className="space-y-2">
        <legend className="text-[13px] font-medium text-fg">Command</legend>
        <p id="exec-arguments-hint" className="text-xs text-fg-muted">
          No shell is used. Each field is passed as one argument exactly as entered, so quotes, pipes and variables are not interpreted. To use a shell, run it explicitly, for example <span className="font-mono">sh</span>, <span className="font-mono">-c</span> and the script as three arguments. Up to {execLimits.maxArguments} arguments, {execLimits.maxArgumentBytes} bytes each and {execLimits.maxTotalBytes / 1024} KiB in total.
        </p>
        <ol className="space-y-2">
          {args.map((argument, index) => (
            <li key={argument.id} className="flex items-center gap-2">
              <label htmlFor={`exec-argument-${argument.id}`} className="w-20 shrink-0 text-xs text-fg-muted">
                {index === 0 ? "Program" : `Argument ${index}`}
              </label>
              <input
                id={`exec-argument-${argument.id}`}
                className={`${inputClass} font-mono`}
                value={argument.value}
                autoComplete="off"
                spellCheck={false}
                aria-describedby="exec-arguments-hint"
                onChange={(event) => update(argument.id, event.target.value)}
              />
              <Button
                variant="ghost"
                size="sm"
                aria-label={index === 0 ? "Remove program" : `Remove argument ${index}`}
                title={index === 0 ? "Remove program" : `Remove argument ${index}`}
                disabled={args.length === 1}
                onClick={() => setArgs((current) => current.filter((item) => item.id !== argument.id))}
              >
                <X aria-hidden className="size-3.5" />
              </Button>
            </li>
          ))}
        </ol>
        <Button size="sm" disabled={args.length >= execLimits.maxArguments} onClick={() => setArgs((current) => [...current, newArgument()])}>
          <Plus aria-hidden className="size-3.5" />
          Add argument
        </Button>
        {submitted && problem && (
          <p role="alert" className="text-xs text-bad">
            {problem}
          </p>
        )}
      </fieldset>

      <div className="grid gap-4 sm:grid-cols-2">
        <Field id="exec-timeout" label="Wait timeout in seconds" error={submitted ? (timeoutError ?? undefined) : undefined} hint={`How long Cove waits for the command and its output, at most ${execLimits.maxTimeoutSeconds} seconds. Reaching it does not guarantee the command stops.`}>
          <input
            id="exec-timeout"
            className={inputClass}
            type="number"
            min={1}
            max={execLimits.maxTimeoutSeconds}
            step={1}
            value={Number.isNaN(timeout) ? "" : timeout}
            aria-invalid={submitted && timeoutError !== null}
            aria-describedby={submitted && timeoutError ? "exec-timeout-error" : "exec-timeout-hint"}
            onChange={(event) => setTimeoutSeconds(event.target.valueAsNumber)}
          />
        </Field>
        <div className="min-w-0 space-y-1.5">
          <p className="text-[13px] font-medium text-fg">Arguments sent</p>
          <p className="rounded-md border border-line bg-panel px-2.5 py-1.5 font-mono text-xs break-all text-fg">{JSON.stringify(command)}</p>
        </div>
      </div>

      {run.isError && (
        <Notice tone="error" title="Could not run the command">
          {errorMessage(run.error)}
        </Notice>
      )}

      <div className="flex flex-wrap justify-end gap-2">
        {(run.data || run.isError) && (
          <Button variant="ghost" onClick={() => run.reset()}>
            Clear output
          </Button>
        )}
        <Button type="submit" variant="primary" disabled={blocked} pending={run.isPending}>
          Run command
        </Button>
      </div>

      {run.isPending && (
        <p role="status" className="text-xs text-fg-muted">
          Waiting up to {timeout} seconds for the command…
        </p>
      )}
      {run.data && <ExecOutput result={run.data} timeout={run.variables.timeout_seconds} />}

      <ConfirmDialog
        open={confirming}
        title="Run this command?"
        confirmLabel="Run command"
        onCancel={() => setConfirming(false)}
        onConfirm={() => {
          setConfirming(false);
          run.mutate(input);
        }}
      >
        <span className="block">
          Runs in service <span className="font-mono text-fg">{service}</span>
          {container ? (
            <>
              {" "}
              container <span className="font-mono text-fg">{container.slice(0, 12)}</span>
            </>
          ) : null}{" "}
          of {workload.name}. Cove waits up to {timeout} seconds for it and keeps at most 64 KiB of output; the command can keep running after that. It can change data inside the container. The audit log records that a command ran, not its arguments or output.
        </span>
        <span className="mt-2 block rounded-md border border-line bg-surface px-2.5 py-1.5 font-mono text-xs break-all text-fg">{JSON.stringify(command)}</span>
      </ConfirmDialog>
    </form>
  );
}
