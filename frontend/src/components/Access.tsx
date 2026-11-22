import { Select } from "./Select";
import { productionEnvironments } from "../lib/hierarchy";
import { useId } from "react";
import type { Project } from "../api/projects";
import type { CustomRole } from "../api/teams";
import type { Grant, GrantRole } from "../api/types";
import { describeScope, scopeValue, parseScope, type GrantRow } from "../lib/grants";
import { grantRoleDescriptions, grantRoleLabels, isBuiltInRole, permissionLabel, roleLabel } from "../lib/permissions";
import { inputClass } from "../lib/styles";
import { Button, Notice } from "./ui";

export function SharedNodeWarning() {
  return (
    <Notice tone="warn" title="Shared nodes are not an isolation boundary">
      Cove enforces dashboard permissions between teams, but resources from different teams on the same node share its Docker engine and kernel.
      Only share a node between teams that trust each other.
    </Notice>
  );
}

export function GrantList({ grants, projects, teamName, customRoles }: { grants: Grant[]; projects: Project[] | undefined; teamName: string; customRoles?: CustomRole[] }) {
  if (grants.length === 0) return <span className="text-fg-subtle">No access</span>;
  return (
    <ul className="space-y-0.5">
      {grants.map((grant) => (
        <li key={`${grant.scope}:${grant.scope_id}`}>
          {roleLabel(grant.role, customRoles)} <span className="text-fg-subtle">·</span> {describeScope(grant, projects, teamName)}
        </li>
      ))}
    </ul>
  );
}

interface GrantsEditorProps {
  teamId: string;
  teamName: string;
  projects: Project[];
  grants: GrantRow[];
  onChange: (grants: GrantRow[]) => void;
  // Omitted or empty when the team has no custom roles or they could not be loaded.
  customRoles?: CustomRole[];
}

// A member sees only what their grants cover; the broadest matching grant applies.
export function GrantsEditor({ teamId, teamName, projects, grants, onChange, customRoles = [] }: GrantsEditorProps) {
  const id = useId();
  const used = new Set(grants.map(scopeValue));
  const options = [
    { value: scopeValue({ scope: "team", scope_id: teamId }), label: `All of ${teamName}` },
    ...projects.flatMap((project) => [
      { value: scopeValue({ scope: "project", scope_id: project.id }), label: `${project.name} (all environments)` },
      ...productionEnvironments(project).map((environment) => ({ value: scopeValue({ scope: "environment", scope_id: environment.id }), label: `${project.name} / ${environment.name}` })),
    ]),
  ];
  const next = options.find((option) => !used.has(option.value));
  const roleOptions = [
    ...(Object.keys(grantRoleLabels) as GrantRole[]).map((role) => ({ value: role, label: grantRoleLabels[role] })),
    ...customRoles.map((role) => ({ value: role.id, label: role.name })),
  ];
  // A grant may name a custom role this editor cannot list (deleted meanwhile, or roles failed to load); keep it selectable.
  const missingRoles = [...new Set(grants.map((grant) => grant.role))].filter((role) => !roleOptions.some((option) => option.value === role));

  return (
    <fieldset className="space-y-3">
      <legend className="text-[13px] font-medium text-fg">Access grants</legend>
      {grants.length === 0 && <p className="text-xs text-fg-muted">Without grants the member can sign in but sees no projects.</p>}
      {grants.map((grant, index) => (
        <div key={grant.key} className="grid gap-2 sm:grid-cols-[minmax(0,1fr)_12rem_auto] sm:items-end">
          <div className="space-y-1">
            <label htmlFor={`${id}-scope-${index}`} className="block text-xs text-fg-muted">
              Scope
            </label>
            <Select
              id={`${id}-scope-${index}`}
              className={inputClass}
              value={scopeValue(grant)}
              onChange={(event) => onChange(grants.map((item, position) => (position === index ? { ...item, ...parseScope(event.target.value) } : item)))}
            >
              {options
                .filter((option) => option.value === scopeValue(grant) || !used.has(option.value))
                .map((option) => (
                  <option key={option.value} value={option.value}>
                    {option.label}
                  </option>
                ))}
            </Select>
          </div>
          <div className="space-y-1">
            <label htmlFor={`${id}-role-${index}`} className="block text-xs text-fg-muted">
              Role
            </label>
            <Select
              id={`${id}-role-${index}`}
              className={inputClass}
              value={grant.role}
              onChange={(event) => onChange(grants.map((item, position) => (position === index ? { ...item, role: event.target.value } : item)))}
            >
              {roleOptions.map((role) => (
                <option key={role.value} value={role.value}>
                  {role.label}
                </option>
              ))}
              {missingRoles.map((role) => (
                <option key={role} value={role}>
                  {roleLabel(role, customRoles)}
                </option>
              ))}
            </Select>
          </div>
          <Button variant="ghost" onClick={() => onChange(grants.filter((_, position) => position !== index))}>
            Remove<span className="sr-only"> grant {index + 1}</span>
          </Button>
        </div>
      ))}
      {next && (
        <Button onClick={() => onChange([...grants, { ...parseScope(next.value), role: "viewer", key: crypto.randomUUID() }])}>
          Add grant
        </Button>
      )}
      <ul className="space-y-0.5 text-xs text-fg-muted">
        {(Object.keys(grantRoleDescriptions) as GrantRole[]).map((role) => (
          <li key={role}>
            <span className="text-fg">{grantRoleLabels[role]}:</span> {grantRoleDescriptions[role]}
          </li>
        ))}
        {customRoles.filter((role) => !isBuiltInRole(role.id)).map((role) => (
          <li key={role.id}>
            <span className="text-fg">{role.name}:</span> {role.permissions.map(permissionLabel).join(", ")}
          </li>
        ))}
      </ul>
    </fieldset>
  );
}
