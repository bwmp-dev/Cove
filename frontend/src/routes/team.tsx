import { Users } from "lucide-react";
import { Checkbox } from "../components/Checkbox";
import { Select } from "../components/Select";
import { SharedVariables } from "../components/SharedVariables";
import { TeamQuota } from "../components/TeamQuota";
import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId, useState, type FormEvent } from "react";
import { errorMessage } from "../api/client";
import { projectsQuery, type Project } from "../api/projects";
import { nodesQuery } from "../api/queries";
import {
  invitationsQuery,
  membersQuery,
  teamNodesQuery,
  useCreateInvitation,
  useCreateTeam,
  useRemoveMember,
  useRevokeInvitation,
  useSetTeamNodes,
  useUpdateMember,
  type Member,
} from "../api/teams";
import type { TeamMembership, TeamRole } from "../api/types";
import { GrantList, GrantsEditor, SharedNodeWarning } from "../components/Access";
import { fromGrantRows, toGrantRows, type GrantRow } from "../lib/grants";
import { ConfirmDialog } from "../components/ConfirmDialog";
import { CopyButton } from "../components/CopyButton";
import { ExpiryTime, RelativeTime } from "../components/RelativeTime";
import { NodeStateLabel } from "../components/status";
import { TeamTimezone } from "../components/TimezoneSettings";
import { Button, EmptyState, Field, LoadingRows, Notice, PageHeader, QueryError, Section, SectionGrid, SettingRows } from "../components/ui";
import { currentTeam, managesTeam, teamRoleDescriptions, teamRoleLabels } from "../lib/permissions";
import { useOnline } from "../lib/hooks";
import { useSession, useSwitchTeam } from "../lib/session";
import { inputClass, listLinkClass, tableHeadClass, tableRowClass } from "../lib/styles";

function RoleSelect({ id, value, onChange, allowOwner }: { id: string; value: TeamRole; onChange: (role: TeamRole) => void; allowOwner: boolean }) {
  return (
    <Field id={id} label="Team role" hint={teamRoleDescriptions[value]}>
      <Select id={id} className={`${inputClass} sm:max-w-xs`} value={value} onChange={(event) => onChange(event.target.value as TeamRole)} aria-describedby={`${id}-hint`}>
        {allowOwner && <option value="owner">{teamRoleLabels.owner}</option>}
        <option value="admin">{teamRoleLabels.admin}</option>
        <option value="member">{teamRoleLabels.member}</option>
      </Select>
    </Field>
  );
}

function MemberEditor({ team, member, projects, onClose }: { team: TeamMembership; member: Member; projects: Project[]; onClose: () => void }) {
  const id = useId();
  const online = useOnline();
  const update = useUpdateMember(team.id);
  const [role, setRole] = useState<TeamRole>(member.role);
  const [grants, setGrants] = useState<GrantRow[]>(() => toGrantRows(member.grants));
  return (
    <form
      className="space-y-4 border-t border-line bg-panel p-4"
      onSubmit={(event) => {
        event.preventDefault();
        update.mutate({ userId: member.user_id, role, grants: role === "member" ? fromGrantRows(grants) : [] }, { onSuccess: onClose });
      }}
    >
      <RoleSelect id={`${id}-role`} value={role} onChange={setRole} allowOwner={team.role === "owner"} />
      {role === "member" && <GrantsEditor teamId={team.id} teamName={team.name} projects={projects} grants={grants} onChange={setGrants} />}
      {update.isError && <Notice tone="error" title="Could not update the member">{errorMessage(update.error)}</Notice>}
      <div className="flex flex-wrap justify-end gap-2">
        <Button variant="ghost" onClick={onClose} disabled={update.isPending}>
          Cancel
        </Button>
        <Button type="submit" variant="primary" disabled={!online} pending={update.isPending}>
          Save access
        </Button>
      </div>
    </form>
  );
}

function Members({ team, projects }: { team: TeamMembership; projects: Project[] }) {
  const session = useSession();
  const members = useQuery(membersQuery(team.id));
  const remove = useRemoveMember(team.id);
  const online = useOnline();
  const [editing, setEditing] = useState<string | null>(null);
  const [removing, setRemoving] = useState<Member | null>(null);

  if (members.isPending) return <LoadingRows label="Loading members" rows={2} />;
  if (members.isError) {
    return (
      <div className="p-3">
        <QueryError what="members" error={members.error} onRetry={() => void members.refetch()} retrying={members.isFetching} />
      </div>
    );
  }
  return (
    <>
      {remove.isError && (
        <div className="p-3">
          <Notice tone="error" title="Could not remove the member">
            {errorMessage(remove.error)}
          </Notice>
        </div>
      )}
      <ul className="divide-y divide-line">
        {members.data.map((member) => {
          const self = member.user_id === session?.user_id;
          // The API refuses to remove the last owner, so explain that instead of offering a failing action.
          const lastOwner = member.role === "owner" && members.data.filter((item) => item.role === "owner").length === 1;
          return (
            <li key={member.user_id}>
              <div className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-3">
                <div className="min-w-0">
                  <p className="text-[13px] font-medium break-words text-fg">
                    {member.name || member.email}
                    {member.user_id === session?.user_id && <span className="ml-2 text-xs font-normal text-fg-subtle">You</span>}
                  </p>
                  <p className="text-xs break-all text-fg-muted">{member.email}</p>
                </div>
                <div className="min-w-0 text-[13px] text-fg-muted sm:max-w-sm">
                  <p className="text-fg">{teamRoleLabels[member.role]}</p>
                  {member.role === "member" ? <GrantList grants={member.grants} projects={projects} teamName={team.name} /> : <p className="text-xs">Full team access</p>}
                </div>
                <div className="flex gap-2">
                  <Button variant="ghost" size="sm" disabled={!online || editing === member.user_id} onClick={() => setEditing(member.user_id)}>
                    Edit<span className="sr-only"> {member.email}</span>
                  </Button>
                  {lastOwner ? (
                    <p className="self-center text-xs text-fg-muted">Last owner, cannot be removed</p>
                  ) : (
                    <Button variant="ghost" size="sm" disabled={!online || remove.isPending} onClick={() => setRemoving(member)}>
                      {self ? "Leave team" : <>Remove<span className="sr-only"> {member.email}</span></>}
                    </Button>
                  )}
                </div>
              </div>
              {editing === member.user_id && <MemberEditor team={team} member={member} projects={projects} onClose={() => setEditing(null)} />}
            </li>
          );
        })}
      </ul>
      <ConfirmDialog
        open={removing !== null}
        title={removing?.user_id === session?.user_id ? `Leave ${team.name}?` : `Remove ${removing?.email ?? "member"}?`}
        confirmLabel={removing?.user_id === session?.user_id ? "Leave team" : "Remove member"}
        danger
        onCancel={() => setRemoving(null)}
        onConfirm={() => {
          if (removing) remove.mutate(removing.user_id);
          setRemoving(null);
        }}
      >
        {removing?.user_id === session?.user_id
          ? "You lose access to this team's projects immediately. Your account and other team memberships are not affected."
          : "They lose access to this team's projects immediately. Their account and other team memberships are not affected. Cove refuses to remove the last owner."}
      </ConfirmDialog>
    </>
  );
}

function InviteForm({ team, projects }: { team: TeamMembership; projects: Project[] }) {
  const id = useId();
  const online = useOnline();
  const create = useCreateInvitation(team.id);
  const [email, setEmail] = useState("");
  const [role, setRole] = useState<"admin" | "member">("member");
  const [grants, setGrants] = useState<GrantRow[]>([]);

  if (create.data) {
    return (
      <div className="space-y-3">
        <Notice tone={create.data.email_sent ? "success" : "info"} title={create.data.email_sent ? "Invitation emailed" : "Email was not sent; share this link privately"}>
          {create.data.email_sent ? "The link below was also emailed. " : ""}
          Treat the link like a password until it expires: whoever opens it can create a new account with it. If {email.trim()} already has an account,
          they must sign in as that address to accept.
        </Notice>
        <Field id={`${id}-link`} label="Invitation link" hint={<>Expires <RelativeTime value={create.data.expires_at} />. It is not shown again.</>}>
          <input id={`${id}-link`} className={`${inputClass} font-mono`} readOnly value={create.data.url} aria-describedby={`${id}-link-hint`} />
        </Field>
        <div className="flex flex-wrap gap-2">
          <CopyButton text={create.data.url} label="Copy link" />
          <Button
            onClick={() => {
              create.reset();
              setEmail("");
              setGrants([]);
            }}
          >
            Done
          </Button>
        </div>
      </div>
    );
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    create.mutate({ email: email.trim(), role, grants: role === "member" ? fromGrantRows(grants) : [] });
  }

  return (
    <form className="space-y-4" onSubmit={submit}>
      <SettingRows>
      <Field layout="row" id={`${id}-email`} label="Email">
        <input id={`${id}-email`} className={inputClass} type="email" required autoComplete="off" value={email} onChange={(event) => setEmail(event.target.value)} />
      </Field>
      <Field layout="row" id={`${id}-role`} label="Team role" hint={teamRoleDescriptions[role]}>
        <Select id={`${id}-role`} className={inputClass} value={role} onChange={(event) => setRole(event.target.value as "admin" | "member")} aria-describedby={`${id}-role-hint`}>
          <option value="member">{teamRoleLabels.member}</option>
          <option value="admin">{teamRoleLabels.admin}</option>
        </Select>
      </Field>
      </SettingRows>
      {role === "member" && <GrantsEditor teamId={team.id} teamName={team.name} projects={projects} grants={grants} onChange={setGrants} />}
      {create.isError && <Notice tone="error" title="Could not create the invitation">{errorMessage(create.error)}</Notice>}
      <div className="flex justify-end border-t border-line pt-4">
        <Button type="submit" variant="primary" disabled={!online} pending={create.isPending}>
          Create invitation
        </Button>
      </div>
    </form>
  );
}

function PendingInvitations({ team, projects }: { team: TeamMembership; projects: Project[] }) {
  const invitations = useQuery(invitationsQuery(team.id));
  const revoke = useRevokeInvitation(team.id);
  const online = useOnline();
  if (invitations.isPending) return <LoadingRows label="Loading invitations" rows={1} />;
  if (invitations.isError) {
    return (
      <div className="p-3">
        <QueryError what="invitations" error={invitations.error} onRetry={() => void invitations.refetch()} retrying={invitations.isFetching} />
      </div>
    );
  }
  if (invitations.data.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No pending invitations.</p>;
  return (
    <>
      {revoke.isError && (
        <div className="p-3">
          <Notice tone="error" title="Could not revoke the invitation">
            {errorMessage(revoke.error)}
          </Notice>
        </div>
      )}
      <ul className="divide-y divide-line">
        {invitations.data.map((invitation) => (
          <li key={invitation.id} className="flex flex-wrap items-start justify-between gap-x-6 gap-y-2 px-4 py-3 text-[13px]">
            <div className="min-w-0">
              <p className="font-medium break-all text-fg">{invitation.email}</p>
              <p className="text-xs text-fg-muted">
                {teamRoleLabels[invitation.role]} · <ExpiryTime value={invitation.expires_at} />
              </p>
            </div>
            {invitation.role === "member" && (
              <div className="min-w-0 text-fg-muted sm:max-w-sm">
                <GrantList grants={invitation.grants} projects={projects} teamName={team.name} />
              </div>
            )}
            <Button variant="ghost" size="sm" disabled={!online} pending={revoke.isPending && revoke.variables === invitation.id} onClick={() => revoke.mutate(invitation.id)}>
              Revoke<span className="sr-only"> invitation for {invitation.email}</span>
            </Button>
          </li>
        ))}
      </ul>
    </>
  );
}

function TeamNodes({ team }: { team: TeamMembership }) {
  const session = useSession();
  const admin = session?.is_admin ?? false;
  // Instance administrators see every node; everyone else is only sent the nodes assigned to the team.
  const assigned = useQuery({ ...teamNodesQuery(team.id), enabled: admin });
  const nodes = useQuery(nodesQuery);
  const save = useSetTeamNodes(team.id);
  const online = useOnline();
  const [selection, setSelection] = useState<Set<string> | null>(null);

  if ((admin && assigned.isPending) || nodes.isPending) return <LoadingRows label="Loading nodes" rows={2} />;
  if (assigned.isError || nodes.isError) {
    const failed = assigned.isError ? assigned : nodes;
    return (
      <div className="p-3">
        <QueryError what="team nodes" error={failed.error} onRetry={() => void failed.refetch()} retrying={failed.isFetching} />
      </div>
    );
  }

  if (selection) {
    return (
      <form
        className="space-y-3 p-4"
        onSubmit={(event) => {
          event.preventDefault();
          save.mutate([...selection], { onSuccess: () => setSelection(null) });
        }}
      >
        <fieldset className="space-y-2">
          <legend className="mb-1 text-[13px] font-medium text-fg">Nodes this team can deploy to</legend>
          {nodes.data.map((node) => (
            <label key={node.id} className="flex items-center gap-2.5 text-[13px]">
              <Checkbox
                checked={selection.has(node.id)}
                onChange={(event) => {
                  const next = new Set(selection);
                  if (event.target.checked) next.add(node.id);
                  else next.delete(node.id);
                  setSelection(next);
                }}
              />
              {node.name}
              <NodeStateLabel state={node.state} />
            </label>
          ))}
        </fieldset>
        {save.isError && <Notice tone="error" title="Could not save team nodes">{errorMessage(save.error)}</Notice>}
        <div className="flex flex-wrap justify-end gap-2">
          <Button variant="ghost" onClick={() => setSelection(null)} disabled={save.isPending}>
            Cancel
          </Button>
          <Button type="submit" variant="primary" disabled={!online} pending={save.isPending}>
            Save nodes
          </Button>
        </div>
      </form>
    );
  }

  const ids = new Set(assigned.data);
  const available = admin ? nodes.data.filter((node) => ids.has(node.id)) : nodes.data;
  return (
    <div>
      {available.length === 0 ? (
        <p className="px-4 py-3 text-[13px] text-fg-muted">No nodes are assigned to this team. {admin ? "Assign nodes so the team can create resources." : "Ask an instance administrator to assign one."}</p>
      ) : (
        <ul className="divide-y divide-line">
          {available.map((node) => (
            <li key={node.id} className="flex flex-wrap items-center justify-between gap-3 px-4 py-2.5 text-[13px]">
              <Link to="/nodes/$nodeId" params={{ nodeId: node.id }} className={listLinkClass}>
                {node.name}
              </Link>
              <NodeStateLabel state={node.state} />
            </li>
          ))}
        </ul>
      )}
      {admin && (
        <div className="flex justify-end border-t border-line px-4 py-3">
          <Button onClick={() => setSelection(new Set(assigned.data))}>Change nodes</Button>
        </div>
      )}
    </div>
  );
}

function YourAccess({ team, projects }: { team: TeamMembership; projects: Project[] | undefined }) {
  return (
    <table className="w-full text-left text-[13px]">
      <caption className="sr-only">Your access</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="py-2 pr-4 pl-4 font-medium">
            Role
          </th>
          <th scope="col" className="py-2 pr-4 font-medium">
            Access
          </th>
        </tr>
      </thead>
      <tbody>
        <tr className={tableRowClass}>
          <td className="py-2.5 pr-4 pl-4 align-top">{teamRoleLabels[team.role]}</td>
          <td className="py-2.5 pr-4 text-fg-muted">{team.role === "member" ? <GrantList grants={team.grants} projects={projects} teamName={team.name} /> : "Full team access, including members and invitations"}</td>
        </tr>
      </tbody>
    </table>
  );
}

function CreateTeam() {
  const id = useId();
  const online = useOnline();
  const create = useCreateTeam();
  const switchTeam = useSwitchTeam();
  const [name, setName] = useState("");
  return (
    <form
      className="space-y-4"
      onSubmit={(event) => {
        event.preventDefault();
        create.mutate(name.trim(), { onSuccess: ({ id: teamId }) => switchTeam(teamId) });
      }}
    >
      <Field layout="row" id={`${id}-name`} label="Team name" hint="You become its owner. Teams do not share projects.">
        <input id={`${id}-name`} className={inputClass} required pattern=".*\S.*" title="Enter a team name" maxLength={120} autoComplete="off" value={name} onChange={(event) => setName(event.target.value)} aria-describedby={`${id}-name-hint`} />
      </Field>
      {create.isError && (
        <Notice tone="error" title="Could not create the team">
          {errorMessage(create.error)}
        </Notice>
      )}
      <div className="flex justify-end border-t border-line pt-4">
        <Button type="submit" disabled={!online} pending={create.isPending}>
          Create team
        </Button>
      </div>
    </form>
  );
}

export function TeamPage() {
  const session = useSession();
  const projects = useQuery(projectsQuery);
  const team = currentTeam(session);
  const member = session?.teams.some((item) => item.id === team?.id) ?? false;

  if (!team) {
    return (
      <>
        <PageHeader icon={<Users aria-hidden />} title="Team" />
        <EmptyState title="You are not in a team yet">Create a team, or accept an invitation from a team owner or admin.</EmptyState>
        <Section title="Create a team">
          <CreateTeam />
        </Section>
      </>
    );
  }

  const manager = managesTeam(team) || Boolean(session?.is_admin);
  const role = member ? teamRoleLabels[team.role] : "Instance administrator";
  return (
    <>
      <PageHeader icon={<Users aria-hidden />} title={team.name} description={`Your role: ${role}. Projects, resources and API tokens belong to the selected team.`} />
      {!manager && (
        <Section title="Your access" flush>
          <YourAccess team={team} projects={projects.data} />
        </Section>
      )}
      {manager && projects.isError && (
        <div className="px-4 pt-5 sm:px-6">
          <QueryError what="projects" error={projects.error} onRetry={() => void projects.refetch()} retrying={projects.isFetching} />
        </div>
      )}
      <SectionGrid>
        {manager && (
          <div className="space-y-5">
            <Section title="Members" description="Owners and admins manage everything in the team. Members only see what their grants cover." flush>
              <Members team={team} projects={projects.data ?? []} />
            </Section>
            <Section title="Invite someone" description="Invitations work even when registration is closed.">
              <InviteForm team={team} projects={projects.data ?? []} />
            </Section>
            <Section title="Pending invitations" flush>
              <PendingInvitations team={team} projects={projects.data ?? []} />
            </Section>
            <SharedVariables scope="team" />
          </div>
        )}
        {/* Two column stacks, so each column flows on its own instead of sharing grid rows. */}
        <div className="space-y-5">
        <Section title="Nodes" description="Instance administrators decide which shared nodes each team can deploy to." flush>
          <div className="border-b border-line p-3">
            <SharedNodeWarning />
          </div>
          <TeamNodes team={team} />
        </Section>
        {manager && <TeamQuota teamId={team.id} admin={Boolean(session?.is_admin)} />}
        {manager && <TeamTimezone key={team.id} teamId={team.id} />}
        <Section title="Create another team">
          <CreateTeam />
        </Section>
        </div>
      </SectionGrid>
    </>
  );
}
