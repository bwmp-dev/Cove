import { describe, expect, it } from "vitest";
import { parseDotenv, sameRoutes, validateComposePath, validateRepository, validateRoutes, validateWorkload, type WorkloadFormValues } from "./workload-form";

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
