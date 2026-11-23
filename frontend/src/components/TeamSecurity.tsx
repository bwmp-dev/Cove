import { useQuery } from "@tanstack/react-query";
import { Plus } from "lucide-react";
import { useId, useState } from "react";
import { errorMessage, isStatus } from "../api/client";
import { secondFactorPolicyQuery, useUpdateSecondFactorPolicy } from "../api/security";
import { rolesQuery, useDeleteRole, useSaveRole, type CustomRole, type TeamRoles } from "../api/teams";
import type { TeamMembership } from "../api/types";
import { useOnline } from "../lib/hooks";
import { grantRoleLabels, permissionLabel, permissionLabels, roleCaveats, sameRolePermissions, setPermission, validateRole, type RoleDraftErrors } from "../lib/permissions";
import { choiceClass, describedBy, inputClass } from "../lib/styles";
import { Checkbox } from "./Checkbox";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button, Field, LoadingRows, Notice, QueryError, Section, SwitchField } from "./ui";

export function TeamSecondFactor({ team, canChange }: { team: TeamMembership; canChange: boolean }) {
  const policy = useQuery(secondFactorPolicyQuery(team.id));
  const update = useUpdateSecondFactorPolicy(team.id);
  const online = useOnline();
  const [confirming, setConfirming] = useState(false);
  return (
    <Section title="Two-step verification" description="Applies to everyone working in this team, in addition to any instance-wide requirement.">
      {policy.isPending ? (
        <p role="status" className="text-[13px] text-fg-muted">
          Loading policy
        </p>
      ) : policy.isError ? (
        <QueryError what="the two-step verification policy" error={policy.error} onRetry={() => void policy.refetch()} retrying={policy.isFetching} />
      ) : (
        <div className="space-y-4">
          <SwitchField
            id={`team-second-factor-${team.id}`}
            layout="row"
            label="Require a second factor"
            description={
              canChange
                ? "Members without an authenticator app or passkey cannot use this team's projects and resources until they set one up."
                : "Only team owners and instance administrators can change this."
            }
            checked={policy.data.require_second_factor}
            disabled={!canChange || !online || update.isPending}
            onChange={(event) => {
              if (event.target.checked) setConfirming(true);
              else update.mutate(false);
            }}
          />
          {update.isError && <Notice tone="error" title="Could not change the policy">{errorMessage(update.error)}</Notice>}
          {update.isSuccess && <Notice tone="success" title={update.data.require_second_factor ? "Second factor required" : "Second factor optional"} />}
        </div>
      )}
      <ConfirmDialog
        open={confirming}
        title="Require a second factor?"
        confirmLabel="Require second factor"
        pending={update.isPending}
        onCancel={() => setConfirming(false)}
        onConfirm={() => update.mutate(true, { onSettled: () => setConfirming(false) })}
      >
        Members who have not set up an authenticator app or passkey are blocked from {team.name}'s projects and resources right away. They can still sign in, enrol
        from their account page and use other teams.
      </ConfirmDialog>
    </Section>
  );
}

function RoleEditor({ teamId, catalogue, builtIn, role, existing, onClose }: { teamId: string; catalogue: string[]; builtIn: TeamRoles["built_in"]; role?: CustomRole; existing: CustomRole[]; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const save = useSaveRole(teamId);
  const [name, setName] = useState(role?.name ?? "");
  const [permissions, setPermissions] = useState<string[]>(() => role?.permissions ?? ["view"]);
  const [errors, setErrors] = useState<RoleDraftErrors>({});
  const duplicate = existing.find((item) => item.id !== role?.id && item.name.trim().toLowerCase() === name.trim().toLowerCase());
  const matching = (Object.keys(builtIn) as (keyof typeof builtIn)[]).find((key) => sameRolePermissions(builtIn[key], permissions));
  const caveats = roleCaveats(permissions, builtIn.editor);
  return (
    <form
      className="space-y-4 border-t border-line bg-panel p-4"
      noValidate
      onSubmit={(event) => {
        event.preventDefault();
        const problems = validateRole(name, permissions, catalogue);
        if (!problems.name && duplicate) problems.name = "Another role already uses this name.";
        setErrors(problems);
        if (problems.name || problems.permissions) return;
        save.mutate({ id: role?.id, name: name.trim(), permissions }, { onSuccess: onClose });
      }}
    >
      <Field id={`${id}-name`} label="Role name" error={errors.name}>
        <input
          id={`${id}-name`}
          className={`${inputClass} sm:max-w-xs`}
          maxLength={120}
          autoComplete="off"
          value={name}
          onChange={(event) => setName(event.target.value)}
          aria-invalid={errors.name ? true : undefined}
          aria-describedby={describedBy(`${id}-name`, errors.name)}
        />
      </Field>
      <fieldset className="space-y-2" aria-describedby={errors.permissions ? `${id}-permissions-error` : undefined}>
        <legend className="mb-1 text-[13px] font-medium text-fg">Permissions</legend>
        <div className="grid gap-2 sm:grid-cols-2">
          {catalogue.map((permission) => (
            <label key={permission} className={`${choiceClass} items-start`}>
              <Checkbox
                className="mt-0.5"
                checked={permissions.includes(permission)}
                disabled={permission === "view"}
                onChange={(event) => setPermissions(setPermission(permissions, permission, event.target.checked, catalogue))}
              />
              <span className="min-w-0">
                <span className="block text-fg">{permissionLabel(permission)}</span>
                {permissionLabels[permission] && <span className="block text-xs text-fg-muted">{permissionLabels[permission].description}</span>}
              </span>
            </label>
          ))}
        </div>
        {errors.permissions && (
          <p id={`${id}-permissions-error`} className="text-xs text-bad">
            {errors.permissions}
          </p>
        )}
      </fieldset>
      {(matching || caveats.length > 0) && (
        <ul className="space-y-1 text-xs text-fg-muted">
          {matching && <li>Same permissions as the built-in {grantRoleLabels[matching]} role.</li>}
          {caveats.map((caveat) => (
            <li key={caveat}>{caveat}</li>
          ))}
        </ul>
      )}
      {save.isError && <Notice tone="error" title="Could not save the role">{errorMessage(save.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={save.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>
          {role ? "Save role" : "Create role"}
        </Button>
      </div>
    </form>
  );
}

export function CustomRoles({ team }: { team: TeamMembership }) {
  const roles = useQuery(rolesQuery(team.id));
  const remove = useDeleteRole(team.id);
  const online = useOnline();
  const [editing, setEditing] = useState<string | null>(null);
  const [deleting, setDeleting] = useState<CustomRole | null>(null);
  return (
    <Section
      title="Roles"
      description="Built-in roles cover most teams. Custom roles combine individual permissions and can be granted wherever built-in roles can."
      flush
      actions={
        roles.isSuccess &&
        editing !== "new" && (
          <Button disabled={!online} onClick={() => setEditing("new")}>
            <Plus aria-hidden className="size-3.5" />
            New role
          </Button>
        )
      }
    >
      {roles.isPending ? (
        <LoadingRows label="Loading roles" rows={2} />
      ) : roles.isError ? (
        <div className="p-3">
          <QueryError what="roles" error={roles.error} onRetry={() => void roles.refetch()} retrying={roles.isFetching} />
        </div>
      ) : (
        <>
          {remove.isError && (
            <div className="p-3">
              <Notice tone="error" title="Could not delete the role">
                {isStatus(remove.error, 409) ? "Members or pending invitations still use this role. Change their grants or revoke the invitations first." : errorMessage(remove.error)}
              </Notice>
            </div>
          )}
          {editing === "new" && (
            <RoleEditor teamId={team.id} catalogue={roles.data.permission_catalogue} builtIn={roles.data.built_in} existing={roles.data.roles} onClose={() => setEditing(null)} />
          )}
          <ul className="divide-y divide-line">
            {(Object.keys(roles.data.built_in) as (keyof TeamRoles["built_in"])[]).map((key) => (
              <li key={key} className="flex flex-wrap items-start justify-between gap-x-6 gap-y-1 px-4 py-2.5">
                <p className="text-[13px] font-medium text-fg">
                  {grantRoleLabels[key]}
                  <span className="ml-2 text-xs font-normal text-fg-subtle">Built in</span>
                </p>
                <p className="min-w-0 text-xs text-fg-muted sm:max-w-sm sm:text-right">{roles.data.built_in[key].map(permissionLabel).join(", ")}</p>
              </li>
            ))}
            {roles.data.roles.map((role) => (
              <li key={role.id}>
                <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-2.5">
                  <div className="min-w-0 flex-1">
                    <p className="text-[13px] font-medium break-words text-fg">{role.name}</p>
                    <p className="text-xs text-fg-muted">{role.permissions.map(permissionLabel).join(", ")}</p>
                  </div>
                  <div className="flex gap-2">
                    <Button variant="ghost" size="sm" disabled={!online || editing === role.id} onClick={() => setEditing(role.id)}>
                      Edit<span className="sr-only"> {role.name}</span>
                    </Button>
                    <Button variant="ghost" size="sm" disabled={!online} pending={remove.isPending && remove.variables === role.id} onClick={() => setDeleting(role)}>
                      Delete<span className="sr-only"> {role.name}</span>
                    </Button>
                  </div>
                </div>
                {editing === role.id && (
                  <RoleEditor teamId={team.id} catalogue={roles.data.permission_catalogue} builtIn={roles.data.built_in} role={role} existing={roles.data.roles} onClose={() => setEditing(null)} />
                )}
              </li>
            ))}
          </ul>
          {roles.data.roles.length === 0 && editing !== "new" && <p className="border-t border-line px-4 py-3 text-[13px] text-fg-muted">No custom roles yet.</p>}
        </>
      )}
      <ConfirmDialog
        open={deleting !== null}
        title={`Delete ${deleting?.name ?? "role"}?`}
        confirmLabel="Delete role"
        danger
        onCancel={() => setDeleting(null)}
        onConfirm={() => {
          if (deleting) remove.mutate(deleting.id);
          setDeleting(null);
        }}
      >
        Cove refuses to delete a role that members or pending invitations still use. Change those grants first.
      </ConfirmDialog>
    </Section>
  );
}
