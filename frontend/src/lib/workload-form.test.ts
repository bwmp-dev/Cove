import { describe, expect, it } from "vitest";
import { parseDotenv, sameRoutes, validateComposePath, validateHostKey, validateRepository, validateRoutes, validateWorkload, type WorkloadFormValues } from "./workload-form";

const base: WorkloadFormValues = {
  name: "web",
  nodeId: "n1",
  kind: "service",
  jobService: "",
  sourceKind: "compose",
  compose: "services: {}",
  repository: "",
  branch: "main",
  composePath: "compose.yaml",
  token: "",
  sshHostKey: "",
  environment: [],
};

describe("validateWorkload", () => {
  it("keeps Cove-managed Compose with its Git source", () => {
    const values = { ...base, sourceKind: "git" as const, repository: "https://github.com/example/app.git", inlineCompose: true };
    const result = validateWorkload(values, "edit");
    expect(result.errors).toEqual({});
    expect(result.input?.compose).toBe(base.compose);
    expect(result.input?.source).toMatchObject({ inline_compose: true, compose_path: "", repository: values.repository });
    expect(validateWorkload({ ...values, compose: "" }, "edit").errors.compose).toBeDefined();
  });
  it("builds a raw Compose service", () => {
    expect(validateWorkload(base, "create").input).toEqual({
      name: "web",
      node_id: "n1",
      kind: "service",
      compose: "services: {}",
      source: null,
      environment: {},
    });
  });

  it("omits environment when editing keeps saved variables", () => {
    const { input } = validateWorkload({ ...base, environment: null }, "edit");
    expect(input).toBeDefined();
    expect(input && "environment" in input).toBe(false);
  });

  it("sends an empty map when every variable is removed", () => {
    expect(validateWorkload({ ...base, environment: [] }, "edit").input?.environment).toEqual({});
  });

  it("builds a Git source and only sends a token when entered", () => {
    const git = { ...base, sourceKind: "git" as const, compose: "ignored", repository: "https://example.com/app.git" };
    expect(validateWorkload(git, "edit").input).toMatchObject({
      compose: "",
      source: { repository: "https://example.com/app.git", branch: "main", compose_path: "compose.yaml" },
    });
    expect(validateWorkload(git, "edit").input?.source).not.toHaveProperty("token");
    expect(validateWorkload({ ...git, token: "secret" }, "edit").input?.source?.token).toBe("secret");
  });

  it("requires a job service for jobs", () => {
    expect(validateWorkload({ ...base, kind: "job" }, "create").errors.jobService).toBeDefined();
    expect(validateWorkload({ ...base, kind: "job", jobService: "migrate" }, "create").input).toMatchObject({ kind: "job", job_service: "migrate" });
  });

  it("flags reserved, duplicate, and malformed variables", () => {
    const { errors } = validateWorkload(
      {
        ...base,
        environment: [
          { id: "a", key: "PATH", value: "" },
          { id: "b", key: "TOKEN", value: "1" },
          { id: "c", key: "TOKEN", value: "2" },
          { id: "d", key: "1BAD", value: "" },
        ],
      },
      "create",
    );
    expect(Object.keys(errors.rows ?? {})).toEqual(["a", "c", "d"]);
  });
});

describe("source validation", () => {
  it("rejects credentials and non-HTTPS repositories", () => {
    expect(validateRepository("https://user:pass@example.com/a.git")).not.toBeNull();
    expect(validateRepository("http://example.com/a.git")).not.toBeNull();
    expect(validateRepository("https://example.com/a.git")).toBeNull();
  });

  it("accepts ssh:// repositories for deploy keys", () => {
    expect(validateRepository("ssh://git@github.com/acme/app.git")).toBeNull();
    expect(validateRepository("ssh://git@git.example.com:2222/acme/app.git")).toBeNull();
    expect(validateRepository("ssh://github.com/acme/app.git")).not.toBeNull();
    expect(validateRepository("ssh://git:secret@github.com/acme/app.git")).not.toBeNull();
    expect(validateRepository("ssh://git@github.com")).not.toBeNull();
    expect(validateRepository("SSH://git@github.com/acme/app.git")).not.toBeNull();
    expect(validateRepository("git@github.com:acme/app.git")).toMatch(/ssh:\/\//);
  });

  it("accepts exactly one pinned host key", () => {
    const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl";
    expect(validateHostKey(key)).toBeNull();
    expect(validateHostKey(`${key} comment`)).toBeNull();
    expect(validateHostKey(`github.com ${key}`)).toMatch(/host name/);
    expect(validateHostKey(`${key}\n${key}`)).not.toBeNull();
    expect(validateHostKey("ssh-ed25519 not-base64!")).not.toBeNull();
    expect(validateHostKey("")).not.toBeNull();
  });

  it("sends the host key instead of a token for SSH sources", () => {
    const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl";
    const ssh = { ...base, sourceKind: "git" as const, repository: "ssh://git@github.com/acme/app.git", sshHostKey: ` ${key} ` };
    const source = validateWorkload(ssh, "edit").input?.source;
    expect(source).toMatchObject({ repository: ssh.repository, ssh_host_key: key });
    expect(source).not.toHaveProperty("token");
    expect(source).not.toHaveProperty("ssh_private_key");
    expect(validateWorkload({ ...ssh, sshHostKey: "" }, "edit").errors.sshHostKey).toBeDefined();
    expect(validateWorkload({ ...ssh, token: "secret" }, "edit").errors.token).toBeDefined();
    const https = validateWorkload({ ...ssh, repository: "https://github.com/acme/app.git" }, "edit").input?.source;
    expect(https).not.toHaveProperty("ssh_host_key");
  });

  it("keeps Compose paths inside the repository", () => {
    expect(validateComposePath("deploy/compose.yaml")).toBeNull();
    expect(validateComposePath("../compose.yaml")).not.toBeNull();
    expect(validateComposePath("a/../../compose.yaml")).not.toBeNull();
    expect(validateComposePath("/etc/compose.yaml")).not.toBeNull();
  });
});

describe("parseDotenv", () => {
  it("parses common .env syntax", () => {
    expect(parseDotenv('# comment\nexport A=1\nB="two words"\nC=\n')).toEqual([
      { key: "A", value: "1" },
      { key: "B", value: "two words" },
      { key: "C", value: "" },
    ]);
  });

  it("returns null for text that is not a .env file", () => {
    expect(parseDotenv("just some text")).toBeNull();
  });
});

describe("route validation", () => {
  const row = (id: string, domain: string, service = "web", port = 80) => ({ id, domain, service, port });

  it("normalises hostnames and reports each invalid row", () => {
    expect(validateRoutes([row("a", " App.Example.com ")])).toEqual({ routes: [{ domain: "app.example.com", service: "web", port: 80 }], errors: {} });
    const result = validateRoutes([
      row("scheme", "https://example.com"),
      row("wildcard", "*.example.com"),
      row("bare", "localhost"),
      row("service", "api.example.com", "bad service"),
      row("port", "port.example.com", "web", 70000),
      row("first", "dup.example.com"),
      row("duplicate", "DUP.example.com"),
    ]);
    expect(result.routes).toBeUndefined();
    expect(Object.keys(result.errors).sort()).toEqual(["bare", "duplicate", "port", "scheme", "service", "wildcard"]);
  });

  it("compares edited rows with saved routes", () => {
    const saved = [{ domain: "app.example.com", service: "web", port: 80 }];
    expect(sameRoutes([row("a", "APP.example.com ")], saved)).toBe(true);
    expect(sameRoutes([], undefined)).toBe(true);
    expect(sameRoutes([row("a", "app.example.com", "web", 8080)], saved)).toBe(false);
    expect(sameRoutes([], saved)).toBe(false);
  });
});

 it("preserves ordered watch patterns and rejects traversal", () => {
  const git = { ...base, sourceKind: "git" as const, repository: "https://github.com/example/app", watchPaths: "src/**\n!**/*.test.ts\nsrc/keep.test.ts" };
  expect(validateWorkload(git, "edit").input?.source?.watch_paths).toEqual(["src/**", "!**/*.test.ts", "src/keep.test.ts"]);
  expect(validateWorkload({ ...git, watchPaths: "../secret" }, "edit").errors.watchPaths).toBeDefined();
 });
