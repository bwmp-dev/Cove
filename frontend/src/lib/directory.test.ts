import { expect, it } from "vitest";
import { ApiError } from "../api/client";
import type { GithubTeamReport, GithubTeamSource } from "../api/directory";
import {
  applyFailure,
  emptyLdapConfiguration,
  githubSourceState,
  ldapConfigurationFromDraft,
  ldapDraft,
  ldapProviderId,
  previewIsCurrent,
  provenanceLabel,
  summarizeReport,
  validateGithubSource,
  validateLdap,
  validateMappings,
  type MappingRow,
} from "./directory";

const row = (group: string, team_id = "team", role = "member", key = group + team_id): MappingRow => ({ key, group, team_id, role });

it("accepts only GitHub team slugs and refuses a duplicate group and team pair", () => {
  const result = validateMappings([row("platform"), row("Platform Team", "team", "member", "b"), row("platform", "team", "admin", "c")], "github");
  expect(result.rows.platform).toBeUndefined();
  expect(result.rows.b?.group).toMatch(/slug/);
  expect(result.rows.c?.team).toBe("This group is already mapped to this team.");
});

it("compares LDAP group DNs without case and requires a DN", () => {
  const result = validateMappings([row("cn=Ops,dc=example"), row("CN=ops,DC=example", "team", "member", "dup"), row("ops", "team", "member", "bad")], "ldap");
  expect(result.rows.dup?.team).toMatch(/already mapped/);
  expect(result.rows.bad?.group).toMatch(/distinguished name/);
});

it("requires a team and role on every mapping and caps the list", () => {
  const rows = Array.from({ length: 101 }, (_, index) => row("", `team-${index}`, "member", `k${index}`));
  const result = validateMappings([...rows, row("", "", "", "empty")], "scim");
  expect(result.list).toBe("Keep at most 100 mappings.");
  expect(result.rows.empty).toEqual({ team: "Choose a team.", role: "Choose a role." });
  expect(result.rows.k0).toBeUndefined();
});

it("requires an installation for App sources but not for OAuth", () => {
  expect(validateGithubSource({ name: "Org", mode: "app", account: "my-org", installation_id: "", enabled: true }).installation_id).toBeDefined();
  expect(validateGithubSource({ name: "Org", mode: "oauth", account: "my-org", installation_id: "", enabled: true })).toEqual({});
  expect(Object.keys(validateGithubSource({ name: " ", mode: "oauth", account: "https://github.com/x", installation_id: "", enabled: true })).sort()).toEqual(["account", "name"]);
});

function report(changes: GithubTeamReport["changes"], version = 3): GithubTeamReport {
  return { version, roster_hash: "a".repeat(64), applied: false, unmatched_github_users: 0, changes };
}

it("summarises additions, removals, changes and warnings", () => {
  const member = { role: "member", grants: [] };
  const summary = summarizeReport(
    report([
      { team_id: "t", user_id: "a", before: null, after: member },
      { team_id: "t", user_id: "b", before: member, after: null, warning: "Last owner kept" },
      { team_id: "t", user_id: "c", before: member, after: { role: "admin", grants: [] } },
    ]),
  );
  expect(summary).toEqual({ added: 1, removed: 1, changed: 1, warnings: 1 });
  expect(summarizeReport(report(null))).toEqual({ added: 0, removed: 0, changed: 0, warnings: 0 });
});

const source = (overrides: Partial<GithubTeamSource> = {}): GithubTeamSource => ({
  id: "s",
  enabled: true,
  version: 3,
  checked_at: null,
  applied_at: null,
  next_check_at: "2026-11-22T10:00:00Z",
  last_error: "",
  credential_configured: true,
  configuration: { name: "Org", mode: "app", account: "my-org", installation_id: 1 },
  mappings: [],
  ...overrides,
});

it("treats a preview as stale once the source revision moves on", () => {
  expect(previewIsCurrent(report([], 3), source())).toBe(true);
  expect(previewIsCurrent(report([], 3), source({ version: 4 }))).toBe(false);
  expect(previewIsCurrent(report([], 3), undefined)).toBe(false);
});

it("classifies apply failures so stale previews are discarded and outages keep access", () => {
  expect(applyFailure(new ApiError(409, "changed"))).toBe("stale");
  expect(applyFailure(new ApiError(502, "github"))).toBe("github");
  expect(applyFailure(new ApiError(503, "down"))).toBe("unavailable");
  expect(applyFailure(new Error("x"))).toBe("other");
});

it("reports the source state without inventing a successful refresh", () => {
  expect(githubSourceState(source()).label).toBe("Waiting for first refresh");
  expect(githubSourceState(source({ checked_at: "2026-11-22T09:00:00Z" })).label).toBe("Syncing");
  expect(githubSourceState(source({ last_error: "rate limited" })).tone).toBe("bad");
  expect(githubSourceState(source({ credential_configured: false, configuration: { name: "Org", mode: "oauth", account: "my-org" } })).label).toBe("Authorization needed");
});

it("labels membership provenance", () => {
  expect(provenanceLabel([])).toBe("Manual");
  expect(provenanceLabel(null)).toBe("Manual");
  expect(provenanceLabel([{ source_kind: "github", source_id: "a" }, { source_kind: "github", source_id: "b" }, { source_kind: "scim", source_id: "" }])).toBe("GitHub team sync and SCIM");
  expect(provenanceLabel([{ source_kind: "github", source_id: "a", display_name: "Engineering (acme)" }, { source_kind: "scim", source_id: "instance", display_name: "SCIM provisioning" }, { source_kind: "ldap", source_id: "d", display_name: "Corp directory" }])).toBe(
    "GitHub team sync — Engineering (acme), SCIM provisioning and LDAP — Corp directory",
  );
  expect(ldapProviderId("ldap:abc")).toBe("abc");
  expect(ldapProviderId("github")).toBeNull();
});

function completeDraft() {
  return {
    ...ldapDraft(emptyLdapConfiguration()),
    display_name: "Corp directory",
    host: "ldap.example.com",
    bind_dn: "cn=cove,dc=example,dc=com",
    user_base_dn: "ou=people,dc=example,dc=com",
    group_base_dn: "ou=groups,dc=example,dc=com",
  };
}

it("requires a bind password only for a new directory and never sends an empty one", () => {
  const draft = completeDraft();
  expect(validateLdap(draft, false).bind_password).toBeDefined();
  expect(validateLdap(draft, true)).toEqual({});
  expect(ldapConfigurationFromDraft(draft)).not.toHaveProperty("bind_password");
  expect(ldapConfigurationFromDraft({ ...draft, bind_password: "s3cret" }).bind_password).toBe("s3cret");
  expect(ldapConfigurationFromDraft(draft).port).toBe(636);
});

it("checks LDAP hosts, filters and placeholders", () => {
  const errors = validateLdap({ ...completeDraft(), host: "ldaps://ldap.example.com", port: "70000", user_filter: "(uid=fixed)", group_filter: "(member={dn}" }, true);
  expect(Object.keys(errors).sort()).toEqual(["group_filter", "host", "port", "user_filter"]);
  expect(validateLdap({ ...completeDraft(), host: "2001:db8::1" }, true).host).toBeUndefined();
});
