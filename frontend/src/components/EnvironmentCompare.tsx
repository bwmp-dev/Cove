import { useQuery } from "@tanstack/react-query";
import { useId, useState } from "react";
import { isStatus } from "../api/client";
import type { Environment } from "../api/projects";
import { environmentComparisonQuery, type EnvironmentComparison } from "../api/project-configuration";
import type { Tone } from "../lib/nodes";
import { shortRevision } from "../lib/project-configuration";
import { tableHeadClass } from "../lib/styles";
import { FieldChanges } from "./ConfigurationChanges";
import { Select } from "./Select";
import { StatusLabel } from "./status";
import { Field, LoadingRows, Notice, QueryError, Section } from "./ui";

const title = "Compare environments";
const description = "Match resources by name across two environments and see which declared fields differ. Values of variables and files are never shown.";

type ResourceState = NonNullable<EnvironmentComparison["resources"]>[number]["state"];

function stateLabel(state: ResourceState, from: string, to: string): { tone: Tone; label: string } {
  switch (state) {
    case "equal":
      return { tone: "ok", label: "Same" };
    case "different":
      return { tone: "warn", label: "Different" };
    case "only_from":
      return { tone: "neutral", label: `Only in ${from}` };
    case "only_to":
      return { tone: "neutral", label: `Only in ${to}` };
  }
}

export function EnvironmentCompare({ projectId, environments }: { projectId: string; environments: readonly Environment[] }) {
  const id = useId();
  const readable = environments.filter((environment) => environment.permissions.includes("read"));
  const [from, setFrom] = useState(readable[0]?.id ?? "");
  const [to, setTo] = useState(readable[1]?.id ?? "");
  const ready = from !== "" && to !== "" && from !== to;
  const comparison = useQuery({ ...environmentComparisonQuery(projectId, from, to), enabled: ready });

  if (readable.length < 2) {
    return (
      <Section title={title} description={description}>
        <p className="text-[13px] text-fg-muted">Comparing needs two environments you can read. Create another environment or ask for access.</p>
      </Section>
    );
  }

  return (
    <Section title={title} description={description} flush>
      <div className="grid gap-4 border-b border-line p-4 sm:grid-cols-2">
        <Field id={`${id}-from`} label="From">
          <Select id={`${id}-from`} value={from} onChange={(event) => setFrom(event.target.value)}>
            {readable.map((environment) => (
              <option key={environment.id} value={environment.id}>
                {environment.name}
              </option>
            ))}
          </Select>
        </Field>
        <Field id={`${id}-to`} label="To">
          <Select id={`${id}-to`} value={to} onChange={(event) => setTo(event.target.value)}>
            {readable.map((environment) => (
              <option key={environment.id} value={environment.id}>
                {environment.name}
              </option>
            ))}
          </Select>
        </Field>
      </div>
      {!ready ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">Choose two different environments.</p>
      ) : comparison.isPending ? (
        <LoadingRows label="Comparing environments" rows={3} />
      ) : comparison.isError ? (
        <div className="p-4">
          {isStatus(comparison.error, 409) ? (
            <Notice tone="warn" title="Resource names must be unique to compare">
              {comparison.error.message} Rename the duplicate resources, then compare again.
            </Notice>
          ) : (
            <QueryError what="the comparison" error={comparison.error} onRetry={() => void comparison.refetch()} retrying={comparison.isFetching} />
          )}
        </div>
      ) : (
        <ComparisonResult comparison={comparison.data} />
      )}
    </Section>
  );
}

function ComparisonResult({ comparison }: { comparison: EnvironmentComparison }) {
  const resources = comparison.resources ?? [];
  const variables = comparison.variable_changes ?? [];
  const differing = resources.filter((resource) => resource.state !== "equal").length;
  return (
    <div className="min-w-0">
      <div className="space-y-2 border-b border-line px-4 py-3 text-[13px]">
        <p role="status">
          {differing === 0 && variables.length === 0 && !comparison.policy_changed
            ? `${comparison.from} and ${comparison.to} declare the same configuration.`
            : `${differing} of ${resources.length} resources differ between ${comparison.from} and ${comparison.to}.`}
        </p>
        <p className="text-xs text-fg-muted">
          Shared variables that differ:{" "}
          {variables.length > 0 ? <span className="font-mono break-all text-fg">{variables.join(", ")}</span> : "none"}
          {" · "}Deployment policy: {comparison.policy_changed ? <span className="text-warn">different</span> : "same"}
        </p>
      </div>
      {resources.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">Neither environment has resources.</p>
      ) : (
        <table className="w-full table-fixed text-left text-[13px]">
          <caption className="sr-only">
            Resources in {comparison.from} compared with {comparison.to}
          </caption>
          <thead>
            <tr className={tableHeadClass}>
              <th scope="col" className="w-2/5 px-4 sm:w-1/4">
                Resource
              </th>
              <th scope="col" className="hidden w-36 px-4 md:table-cell">
                State
              </th>
              <th scope="col" className="px-4">
                Differences
              </th>
              <th scope="col" className="hidden w-48 px-4 lg:table-cell">
                Deployed versions
              </th>
            </tr>
          </thead>
          <tbody>
            {resources.map((resource) => {
              const status = stateLabel(resource.state, comparison.from, comparison.to);
              return (
                <tr key={resource.name} className="border-b border-line align-top last:border-b-0">
                  <td className="px-4 py-2.5">
                    <p className="font-medium break-words text-fg">{resource.name}</p>
                    <div className="mt-1 md:hidden">
                      <StatusLabel tone={status.tone} label={status.label} />
                    </div>
                  </td>
                  <td className="hidden px-4 py-2.5 md:table-cell">
                    <StatusLabel tone={status.tone} label={status.label} />
                  </td>
                  <td className="min-w-0 px-4 py-2.5">
                    {resource.state === "different" ? <FieldChanges fields={resource.fields} diffs={resource.diffs} /> : <span className="text-xs text-fg-muted">{resource.state === "equal" ? "No differences" : "Not declared in both"}</span>}
                    <p className="mt-1 text-xs text-fg-muted lg:hidden">
                      <Versions comparison={comparison} from={resource.from_revision} to={resource.to_revision} />
                    </p>
                  </td>
                  <td className="hidden px-4 py-2.5 text-xs text-fg-muted lg:table-cell">
                    <Versions comparison={comparison} from={resource.from_revision} to={resource.to_revision} />
                  </td>
                </tr>
              );
            })}
          </tbody>
        </table>
      )}
    </div>
  );
}

function Versions({ comparison, from, to }: { comparison: EnvironmentComparison; from: string; to: string }) {
  if (!from && !to) return <>Not deployed</>;
  return (
    <>
      {comparison.from}{" "}
      <span className="font-mono text-fg" title={from || undefined}>
        {from ? shortRevision(from) : "none"}
      </span>
      {", "}
      {comparison.to}{" "}
      <span className="font-mono text-fg" title={to || undefined}>
        {to ? shortRevision(to) : "none"}
      </span>
    </>
  );
}
