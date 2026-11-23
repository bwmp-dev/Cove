import { ApiError } from "../api/client";
import type { DirectoryChange, DirectoryMapping, DirectoryTeam, GithubTeamReport, GithubTeamSource, GithubTeamSourceInput, LdapConfiguration, MemberState } from "../api/directory";
import type { MembershipSource } from "../api/teams";
import { grantRoleLabels, roleLabel, teamRoleLabels } from "./permissions";

export const maxMappings = 100;

export const directoryRoles = [
  { value: "owner", label: teamRoleLabels.owner, description: "Full control of the team, including its owners." },
  { value: "admin", label: teamRoleLabels.admin, description: "Manages everything in the team except owners." },
  { value: "member", label: `${teamRoleLabels.member} without access`, description: "Joins the team; grants come from other sources or by hand." },
  { value: "viewer", label: `${grantRoleLabels.viewer} (team-wide)`, description: "Member with read access to every project." },
  { value: "operator", label: `${grantRoleLabels.operator} (team-wide)`, description: "Member who can read and operate every resource." },
  { value: "editor", label: `${grantRoleLabels.editor} (team-wide)`, description: "Member who can change every project's configuration." },
] as const;

export function directoryRoleLabel(role: string, customRoles?: readonly { id: string; name: string }[]) {
  const builtIn = directoryRoles.find((item) => item.value === role);
  if (builtIn) return builtIn.label;
  return customRoles?.find((item) => item.id === role)?.name ?? "Custom role";
}

export function teamName(teams: readonly DirectoryTeam[] | undefined, id: string) {
  return teams?.find((team) => team.id === id)?.name;
}

export type MappingRow = DirectoryMapping & { key: string };

let nextKey = 0;
export function mappingKey() {
  nextKey += 1;
  return `mapping-${nextKey}`;
}

export function toMappingRows(mappings: readonly DirectoryMapping[] | null | undefined): MappingRow[] {
  return (mappings ?? []).map((mapping) => ({ ...mapping, key: mappingKey() }));
}

export function fromMappingRows(rows: readonly MappingRow[]): DirectoryMapping[] {
  return rows.map(({ group, team_id, role }) => ({ group: group.trim(), team_id, role }));
}

export function sameMappings(left: readonly DirectoryMapping[], right: readonly DirectoryMapping[]) {
  const text = (items: readonly DirectoryMapping[]) => items.map((item) => `${item.group}\u0000${item.team_id}\u0000${item.role}`).sort().join("\u0001");
  return text(left) === text(right);
}

// "github": GitHub team slugs. "ldap": group DNs. "scim": the group is fixed by the provisioned Group, so only teams are checked.
export type GroupKind = "github" | "ldap" | "scim";

export type MappingErrors = Record<string, { group?: string; team?: string; role?: string }>;

const teamSlug = /^[a-z0-9][a-z0-9_-]{0,99}$/;

// A DN is at least one attribute=value pair; the API parses it fully and reports anything this misses.
const looksLikeDN = /^\s*[A-Za-z][\w.-]*\s*=\s*[^,]+/;

export function validateMappings(rows: readonly MappingRow[], kind: GroupKind): { rows: MappingErrors; list?: string } {
  const errors: MappingErrors = {};
  const seen = new Map<string, string>();
  for (const row of rows) {
    const problem: MappingErrors[string] = {};
    const group = row.group.trim();
    if (kind === "github") {
      if (group === "") problem.group = "Enter a GitHub team slug.";
      else if (!teamSlug.test(group)) problem.group = "Use the team's slug: lowercase letters, digits, hyphens or underscores, as in its GitHub URL.";
    } else if (kind === "ldap") {
      if (group === "") problem.group = "Enter the group's distinguished name.";
      else if (!looksLikeDN.test(group) || group.length > 2048) problem.group = "Enter a distinguished name such as cn=deployers,ou=groups,dc=example,dc=com.";
    }
    if (row.team_id === "") problem.team = "Choose a team.";
    if (row.role === "") problem.role = "Choose a role.";
    const pair = `${kind === "ldap" ? group.toLowerCase() : group}\u0000${row.team_id}`;
    if (row.team_id !== "" && seen.has(pair)) problem.team = kind === "scim" ? "This team is already mapped." : "This group is already mapped to this team.";
    seen.set(pair, row.key);
    if (Object.keys(problem).length > 0) errors[row.key] = problem;
  }
  return { rows: errors, list: rows.length > maxMappings ? `Keep at most ${maxMappings} mappings.` : undefined };
}

export function hasMappingErrors(result: ReturnType<typeof validateMappings>) {
  return result.list !== undefined || Object.keys(result.rows).length > 0;
}

export function utf8Length(value: string) {
  return new TextEncoder().encode(value).length;
}

const githubAccount = /^[a-zA-Z0-9][a-zA-Z0-9-]{0,38}$/;

export interface GithubSourceDraft {
  name: string;
  mode: "app" | "oauth";
  account: string;
  installation_id: string;
  enabled: boolean;
}

export type GithubSourceErrors = Partial<Record<"name" | "account" | "installation_id", string>>;

export function validateGithubSource(draft: GithubSourceDraft): GithubSourceErrors {
  const errors: GithubSourceErrors = {};
  const name = draft.name.trim();
  if (name === "" || utf8Length(name) > 120) errors.name = "Enter a name of up to 120 characters.";
  if (!githubAccount.test(draft.account.trim())) errors.account = "Enter the GitHub organization's login, as in github.com/<organization>.";
  if (draft.mode === "app" && draft.installation_id === "") errors.installation_id = "Choose the GitHub App installation on this organization.";
  return errors;
}

// The App installation must belong to the organization being synced.
export function installationMismatch(draft: GithubSourceDraft, installation: { account: string } | undefined) {
  if (draft.mode !== "app" || !installation || installation.account.toLowerCase() === draft.account.trim().toLowerCase()) return undefined;
  return `This installation belongs to ${installation.account}, not ${draft.account.trim() || "the organization above"}.`;
}

export function githubSourceInput(draft: GithubSourceDraft, rows: readonly MappingRow[], version: number | undefined): GithubTeamSourceInput {
  return {
    name: draft.name.trim(),
    mode: draft.mode,
    account: draft.account.trim(),
    ...(draft.mode === "app" ? { installation_id: Number(draft.installation_id) } : {}),
    enabled: draft.enabled,
    ...(version !== undefined ? { version } : {}),
    mappings: fromMappingRows(rows),
  };
}

export type ChangeKind = "added" | "removed" | "changed";

export function changeKind(change: DirectoryChange): ChangeKind {
  if (change.before === null) return "added";
  if (change.after === null) return "removed";
  return "changed";
}

export function memberStateLabel(state: MemberState, customRoles?: readonly { id: string; name: string }[]) {
  if (state.role !== "member") return teamRoleLabels[state.role as keyof typeof teamRoleLabels] ?? state.role;
  if (state.grants.length === 0) return "Member without access";
  const roles = state.grants.map((grant) => `${roleLabel(grant.role, customRoles)}${grant.scope === "team" ? " (team-wide)" : ` on one ${grant.scope}`}`);
  return `Member: ${roles.join(", ")}`;
}

export interface ReportSummary {
  added: number;
  removed: number;
  changed: number;
  warnings: number;
}

export function summarizeReport(report: GithubTeamReport): ReportSummary {
  const summary: ReportSummary = { added: 0, removed: 0, changed: 0, warnings: 0 };
  for (const change of report.changes ?? []) {
    summary[changeKind(change)] += 1;
    if (change.warning) summary.warnings += 1;
  }
  return summary;
}

export function plural(count: number, one: string, many = `${one}s`) {
  return `${count} ${count === 1 ? one : many}`;
}

// A preview is only valid for the source revision it was taken from. Saving the source, or the background
// refresh applying a change, advances the version; the API would refuse to apply it, so the page refuses first.
export function previewIsCurrent(report: GithubTeamReport, source: GithubTeamSource | undefined) {
  return source !== undefined && source.version === report.version;
}

export type ApplyFailure = "stale" | "github" | "unavailable" | "other";

export function applyFailure(error: unknown): ApplyFailure {
  if (error instanceof ApiError) {
    if (error.status === 409 || error.status === 400) return "stale";
    if (error.status === 502) return "github";
    if (error.status === 503) return "unavailable";
  }
  return "other";
}

export type GithubSourceState = { tone: "ok" | "warn" | "bad" | "neutral" | "info"; label: string };

export function githubSourceState(source: GithubTeamSource): GithubSourceState {
  if (!source.credential_configured) return { tone: "warn", label: source.configuration.mode === "oauth" ? "Authorization needed" : "Connection needed" };
  if (source.last_error !== "") return { tone: "bad", label: "Last refresh failed" };
  if (!source.enabled) return { tone: "neutral", label: "Paused" };
  if (source.checked_at === null) return { tone: "info", label: "Waiting for first refresh" };
  return { tone: "ok", label: "Syncing" };
}

export const sourceKindLabels: Record<MembershipSource["source_kind"], string> = { github: "GitHub team sync", scim: "SCIM", ldap: "LDAP" };

// Instance administrators receive each source's display name; other members see only the kind of source.
function sourceLabel(source: MembershipSource) {
  const name = source.display_name?.trim();
  if (source.source_kind === "scim") return name || sourceKindLabels.scim;
  return name ? `${sourceKindLabels[source.source_kind]} — ${name}` : sourceKindLabels[source.source_kind];
}

export function provenanceLabel(sources: readonly MembershipSource[] | null | undefined) {
  const labels = [...new Set((sources ?? []).map(sourceLabel))];
  if (labels.length === 0) return "Manual";
  if (labels.length === 1) return labels[0] ?? "Manual";
  return `${labels.slice(0, -1).join(", ")} and ${labels.at(-1) ?? ""}`;
}

export function isManaged(sources: readonly MembershipSource[] | null | undefined) {
  return (sources ?? []).length > 0;
}

export const ldapIdentityPrefix = "ldap:";

export function ldapProviderId(identityProvider: string) {
  return identityProvider.startsWith(ldapIdentityPrefix) ? identityProvider.slice(ldapIdentityPrefix.length) : null;
}

export const ldapPorts = { ldaps: 636, starttls: 389 } as const;

// Suggested starting points the administrator edits. Host, base DNs and credentials are never invented.
export function emptyLdapConfiguration(): LdapConfiguration {
  return {
    display_name: "",
    host: "",
    port: ldapPorts.ldaps,
    transport: "ldaps",
    ca_pem: "",
    bind_dn: "",
    user_base_dn: "",
    user_filter: "(&(objectClass=inetOrgPerson)(uid={username}))",
    subject_attribute: "entryUUID",
    email_attribute: "mail",
    name_attribute: "cn",
    group_base_dn: "",
    group_filter: "(&(objectClass=groupOfNames)(member={dn}))",
    allow_registration: false,
  };
}

export interface LdapDraft extends Omit<LdapConfiguration, "port" | "bind_password"> {
  port: string;
  bind_password: string;
}

export type LdapErrors = Partial<Record<keyof LdapDraft, string>>;

const attributeName = /^[A-Za-z][A-Za-z0-9-]{0,63}$/;

function balanced(filter: string) {
  let depth = 0;
  for (const character of filter) {
    if (character === "(") depth += 1;
    if (character === ")") depth -= 1;
    if (depth < 0) return false;
  }
  return depth === 0 && filter.startsWith("(");
}

export function validateLdap(draft: LdapDraft, passwordSaved: boolean): LdapErrors {
  const errors: LdapErrors = {};
  const name = draft.display_name.trim();
  if (name === "" || utf8Length(name) > 120) errors.display_name = "Enter the name shown on the sign-in page, up to 120 characters.";
  const host = draft.host.trim();
  if (host === "" || host.length > 253 || /[/\\@\s]/.test(host) || (host.includes(":") && !/^[0-9a-fA-F:.]+$/.test(host)))
    errors.host = "Enter the directory server's host name only, without ldaps:// or a port.";
  const port = Number(draft.port);
  if (!Number.isInteger(port) || port < 1 || port > 65535) errors.port = "Enter a port from 1 to 65535.";
  if (draft.ca_pem.trim() !== "" && !draft.ca_pem.includes("-----BEGIN CERTIFICATE-----")) errors.ca_pem = "Paste PEM certificates that start with -----BEGIN CERTIFICATE-----.";
  for (const key of ["bind_dn", "user_base_dn", "group_base_dn"] as const) {
    if (!looksLikeDN.test(draft[key])) errors[key] = "Enter a distinguished name such as dc=example,dc=com.";
  }
  if (draft.bind_password === "" && !passwordSaved) errors.bind_password = "Enter the service account's password.";
  if (utf8Length(draft.bind_password) > 4096) errors.bind_password = "The password is too long.";
  if (!draft.user_filter.includes("{username}")) errors.user_filter = "Include {username} where the typed directory username goes.";
  else if (!balanced(draft.user_filter)) errors.user_filter = "Enter an LDAP filter in parentheses, such as (uid={username}).";
  if (!draft.group_filter.includes("{dn}") && !draft.group_filter.includes("{username}")) errors.group_filter = "Include {dn} (the user's DN) or {username}.";
  else if (!balanced(draft.group_filter)) errors.group_filter = "Enter an LDAP filter in parentheses, such as (member={dn}).";
  for (const key of ["email_attribute", "name_attribute"] as const) {
    if (!attributeName.test(draft[key])) errors[key] = "Enter an attribute name such as mail or cn.";
  }
  return errors;
}

export function ldapDraft(configuration: LdapConfiguration): LdapDraft {
  return { ...configuration, port: String(configuration.port), bind_password: "" };
}

// An empty bind password is omitted, so an update keeps the saved one.
export function ldapConfigurationFromDraft(draft: LdapDraft): LdapConfiguration {
  const { bind_password, port, ...rest } = draft;
  return {
    ...rest,
    display_name: draft.display_name.trim(),
    host: draft.host.trim(),
    port: Number(port),
    ca_pem: draft.ca_pem.trim(),
    bind_dn: draft.bind_dn.trim(),
    user_base_dn: draft.user_base_dn.trim(),
    user_filter: draft.user_filter.trim(),
    email_attribute: draft.email_attribute.trim(),
    name_attribute: draft.name_attribute.trim(),
    group_base_dn: draft.group_base_dn.trim(),
    group_filter: draft.group_filter.trim(),
    ...(bind_password !== "" ? { bind_password } : {}),
  };
}

export const ldapMalformedMessage = "Enter a directory username and password without line breaks or control characters (usernames up to 254 bytes).";

export function ldapSignInErrorMessage(error: unknown, fallback: (error: unknown) => string) {
  if (error instanceof ApiError) {
    if (error.status === 401) return "Incorrect directory username or password.";
    if (error.status === 429) {
      if ((error.retryAfter ?? 0) >= 900 || error.message.toLowerCase().includes("later")) return "Sign-in is paused after repeated failed attempts. Try again in 15 minutes.";
      return `Too many attempts. Try again in ${error.retryAfter ?? 60} seconds.`;
    }
    if (error.status === 404) return "This directory is no longer available for sign-in. Reload the page.";
    if (error.status === 400) return ldapMalformedMessage;
  }
  return fallback(error);
}
