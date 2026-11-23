import { useQuery } from "@tanstack/react-query";
import { Plus, X } from "lucide-react";
import { directoryTeamsQuery, type DirectoryMapping, type DirectoryTeam } from "../api/directory";
import { rolesQuery } from "../api/teams";
import { directoryRoleLabel, directoryRoles, mappingKey, maxMappings, teamName, type GroupKind, type MappingErrors, type MappingRow } from "../lib/directory";
import { describedBy, inputClass } from "../lib/styles";
import { Select } from "./Select";
import { Button, Field } from "./ui";

const groupFields: Record<Exclude<GroupKind, "scim">, { label: string; placeholder: string; hint: string }> = {
  github: { label: "GitHub team slug", placeholder: "platform-team", hint: "From the team's URL: github.com/orgs/<organization>/teams/<slug>." },
  ldap: { label: "Group DN", placeholder: "cn=deployers,ou=groups,dc=example,dc=com", hint: "The group's full distinguished name, as returned by the group search." },
};

// The listing covers every team, so an ID it lacks is a team removed since (or a listing that failed to load).
function TeamLabel({ teams, id }: { teams: readonly DirectoryTeam[] | undefined; id: string }) {
  const name = teamName(teams, id);
  if (name) return <>{name}</>;
  return (
    <span title={id}>
      {teams ? "Removed team" : "Team"} <span className="font-mono text-xs">{id}</span>
    </span>
  );
}

function RoleLabel({ teamId, role }: { teamId: string; role: string }) {
  const roles = useQuery({ ...rolesQuery(teamId), enabled: !directoryRoles.some((item) => item.value === role) });
  return <>{directoryRoleLabel(role, roles.data?.roles)}</>;
}

// Read-only list of saved mappings, for summaries outside the editor.
export function MappingList({ mappings, showGroup = true }: { mappings: readonly DirectoryMapping[]; showGroup?: boolean }) {
  const teams = useQuery(directoryTeamsQuery);
  if (mappings.length === 0) return <p className="text-xs text-fg-muted">No mappings. Nobody gains team access from this source.</p>;
  return (
    <ul className="space-y-0.5 text-xs text-fg-muted">
      {mappings.map((mapping) => (
        <li key={`${mapping.group}:${mapping.team_id}`} className="break-words">
          {showGroup && (
            <>
              <span className="font-mono break-all text-fg">{mapping.group}</span>
              <span className="text-fg-subtle"> → </span>
            </>
          )}
          <TeamLabel teams={teams.data} id={mapping.team_id} />
          <span className="text-fg-subtle"> · </span>
          <RoleLabel teamId={mapping.team_id} role={mapping.role} />
        </li>
      ))}
    </ul>
  );
}

function MappingRowFields({
  id,
  index,
  row,
  kind,
  teams,
  errors,
  onChange,
  onRemove,
}: {
  id: string;
  index: number;
  row: MappingRow;
  kind: GroupKind;
  teams: readonly DirectoryTeam[];
  errors: MappingErrors[string] | undefined;
  onChange: (row: MappingRow) => void;
  onRemove: () => void;
}) {
  const roles = useQuery({ ...rolesQuery(row.team_id), enabled: row.team_id !== "" });
  const custom = roles.data?.roles ?? [];
  const unknownRole = row.role !== "" && !directoryRoles.some((item) => item.value === row.role) && !custom.some((item) => item.id === row.role);
  const unknownTeam = row.team_id !== "" && !teams.some((team) => team.id === row.team_id);
  const field = kind === "scim" ? null : groupFields[kind];
  const rowId = `${id}-${row.key}`;
  const roleHint = directoryRoles.find((item) => item.value === row.role)?.description;
  return (
    <li className="grid min-w-0 gap-3 py-3 first:pt-0 last:pb-0 sm:grid-cols-[minmax(0,1fr)_auto] sm:items-start">
      <fieldset className={`grid min-w-0 gap-3 ${field ? "@2xl:grid-cols-[minmax(0,3fr)_minmax(0,2fr)_minmax(0,2fr)]" : "@lg:grid-cols-2"}`}>
        <legend className="sr-only">Mapping {index + 1}</legend>
        {field && (
          <Field id={`${rowId}-group`} label={field.label} error={errors?.group}>
            <input
              id={`${rowId}-group`}
              className={`${inputClass} font-mono`}
              autoComplete="off"
              spellCheck={false}
              placeholder={field.placeholder}
              value={row.group}
              onChange={(event) => onChange({ ...row, group: event.target.value })}
              aria-invalid={errors?.group ? true : undefined}
              aria-describedby={describedBy(`${rowId}-group`, errors?.group)}
            />
          </Field>
        )}
        <Field id={`${rowId}-team`} label="Cove team" error={errors?.team}>
          <Select
            id={`${rowId}-team`}
            className={inputClass}
            value={row.team_id}
            onChange={(event) => onChange({ ...row, team_id: event.target.value, role: directoryRoles.some((item) => item.value === row.role) ? row.role : "member" })}
            aria-invalid={errors?.team ? true : undefined}
            aria-describedby={describedBy(`${rowId}-team`, errors?.team)}
          >
            <option value="" disabled>
              Choose a team
            </option>
            {teams.map((team) => (
              <option key={team.id} value={team.id}>
                {team.name}
              </option>
            ))}
            {unknownTeam && <option value={row.team_id}>{`Removed team (${row.team_id})`}</option>}
          </Select>
        </Field>
        <Field id={`${rowId}-role`} label="Role" hint={roleHint} error={errors?.role}>
          <Select
            id={`${rowId}-role`}
            className={inputClass}
            value={row.role}
            onChange={(event) => onChange({ ...row, role: event.target.value })}
            aria-invalid={errors?.role ? true : undefined}
            aria-describedby={describedBy(`${rowId}-role`, errors?.role, roleHint)}
          >
            {directoryRoles.map((role) => (
              <option key={role.value} value={role.value}>
                {role.label}
              </option>
            ))}
            {custom.length > 0 && (
              <optgroup label="Custom roles (team-wide)">
                {custom.map((role) => (
                  <option key={role.id} value={role.id}>
                    {role.name}
                  </option>
                ))}
              </optgroup>
            )}
            {unknownRole && <option value={row.role}>Custom role</option>}
          </Select>
        </Field>
      </fieldset>
      <Button variant="ghost" size="sm" className="justify-self-end sm:mt-6" aria-label={`Remove mapping ${index + 1}`} title="Remove mapping" onClick={onRemove}>
        <X aria-hidden className="size-3.5" />
        <span className="sm:hidden">Remove</span>
      </Button>
    </li>
  );
}

interface MappingEditorProps {
  id: string;
  kind: GroupKind;
  rows: MappingRow[];
  onChange: (rows: MappingRow[]) => void;
  errors: MappingErrors;
  listError?: string;
  // The fixed group for SCIM mappings, whose group is the provisioned Group itself.
  fixedGroup?: string;
}

export function MappingEditor({ id, kind, rows, onChange, errors, listError, fixedGroup = "" }: MappingEditorProps) {
  const teams = useQuery(directoryTeamsQuery);
  const available = teams.data ?? [];
  return (
    <div className="space-y-3">
      {teams.isError && <p className="text-xs text-bad">Teams could not be loaded, so new mappings cannot choose a team. Reload to try again.</p>}
      {rows.length === 0 ? (
        <p className="text-xs text-fg-muted">No mappings yet.</p>
      ) : (
        <ul aria-label="Mappings" className="divide-y divide-line">
          {rows.map((row, index) => (
            <MappingRowFields
              key={row.key}
              id={id}
              index={index}
              row={row}
              kind={kind}
              teams={available}
              errors={errors[row.key]}
              onChange={(next) => onChange(rows.map((item) => (item.key === row.key ? next : item)))}
              onRemove={() => onChange(rows.filter((item) => item.key !== row.key))}
            />
          ))}
        </ul>
      )}
      {listError && <p className="text-xs text-bad">{listError}</p>}
      <div className="flex flex-wrap items-center gap-3">
        <Button size="sm" disabled={rows.length >= maxMappings || available.length === 0} onClick={() => onChange([...rows, { key: mappingKey(), group: fixedGroup, team_id: available.length === 1 ? (available[0]?.id ?? "") : "", role: "member" }])}>
          <Plus aria-hidden className="size-3.5" />
          Add mapping
        </Button>
        <p className="text-xs text-fg-muted">Access from several mappings and sources is combined.</p>
      </div>
    </div>
  );
}
