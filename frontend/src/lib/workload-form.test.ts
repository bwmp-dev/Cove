import { describe, expect, it } from "vitest";
import type { Route } from "../api/types";
import { emptyForwardAuth, emptyPolicy, extensionSummary, isCIDR, parseDotenv, routeRows, sameRoutes, validateComposePath, validateHostKey, validateRepository, validateRoutes, validateWorkload, withoutAuthSecrets, type PolicyDraft, type RouteRow, type WorkloadFormValues } from "./workload-form";

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

  const auth = "admin:$2y$10$" + "a".repeat(53);
  const policyRoute: Route = {
    domain: "app.example.com",
    service: "web",
    port: 8080,
    auth,
    policy: {
      path_prefix: "/api",
      request_headers: { "X-App": "api" },
      response_headers: { "X-Version": "1", Server: "" },
      security_headers: true,
      ip_allow_list: ["10.0.0.0/8", "fd00::/8"],
      redirect_host: "www.example.com",
      redirect_permanent: true,
      certificate_source: "acme",
      upstreams: [{ service: "api", port: 8081 }],
      sticky: true,
    },
  };

  it("round-trips saved policies and basic auth unchanged", () => {
    const rows = routeRows([policyRoute, { domain: "plain.example.com", service: "web", port: 80 }]);
    expect(sameRoutes(rows, [policyRoute, { domain: "plain.example.com", service: "web", port: 80 }])).toBe(true);
    expect(validateRoutes(rows)).toEqual({ routes: [policyRoute, { domain: "plain.example.com", service: "web", port: 80 }], errors: {} });
  });

  it("treats header order as unchanged and value edits as changes", () => {
    const [row] = routeRows([policyRoute]) as [RouteRow];
    const policy = row.policy ?? emptyPolicy();
    expect(sameRoutes([{ ...row, policy: { ...policy, responseHeaders: [...policy.responseHeaders].reverse() } }], [policyRoute])).toBe(true);
    expect(sameRoutes([{ ...row, policy: { ...policy, sticky: false } }], [policyRoute])).toBe(false);
  });

  it("omits empty policies so legacy routes keep their shape", () => {
    const result = validateRoutes([{ ...row("a", "app.example.com"), policy: { ...emptyPolicy(), redirectPermanent: true } }]);
    expect(result.routes).toEqual([{ domain: "app.example.com", service: "web", port: 80 }]);
  });

  it("allows one hostname on several path prefixes but not the same pair twice", () => {
    const path = (id: string, pathPrefix: string): RouteRow => ({ ...row(id, "app.example.com"), policy: { ...emptyPolicy(), pathPrefix } });
    expect(validateRoutes([row("root", "app.example.com"), path("api", "/api")]).errors).toEqual({});
    expect(Object.keys(validateRoutes([path("a", "/api"), path("b", "/api")]).errors)).toEqual(["b"]);
  });

  it("reports invalid policy fields next to the field", () => {
    const header = (id: string, name: string, value = "x") => ({ id, name, value });
    const result = validateRoutes([
      {
        ...row("r", "app.example.com"),
        policy: {
          ...emptyPolicy(),
          pathPrefix: "api?x=1",
          redirectHost: "app.example.com",
          securityHeaders: true,
          requestHeaders: [header("host", "Host"), header("fwd", "X-Forwarded-For"), header("ok", "X-App"), header("dup", "x-app")],
          responseHeaders: [header("preset", "X-Frame-Options"), header("line", "X-Line", "a\nb"), header("tab", "X-Tab", "a\tb"), header("del", "X-Del", "a\u007fb")],
          ipAllowList: "10.0.0.0/8\n10.0.0.1",
          upstreams: [{ id: "same", service: "web", port: 80 }, { id: "port", service: "api", port: 0 }],
        },
      },
    ]);
    expect(result.routes).toBeUndefined();
    expect(Object.keys(result.errors).sort()).toEqual(["r.cidr", "r.path", "r.redirect", "r.request.dup", "r.request.fwd", "r.request.host", "r.response.del", "r.response.line", "r.response.preset", "r.response.tab", "r.upstream.port", "r.upstream.same"]);
  });

  it("accepts the CIDR forms the worker accepts", () => {
    for (const cidr of ["10.0.0.0/8", "0.0.0.0/0", "192.168.1.1/32", "fd00::/8", "::/0", "2001:db8::1/128", "::ffff:10.0.0.1/128"]) expect(isCIDR(cidr)).toBe(true);
    for (const cidr of ["10.0.0.0", "10.0.0.0/33", "010.0.0.0/8", "10.0.0/8", "fd00::/129", "fe80::1%eth0/64", "1:2:3:4:5:6:7:8:9/64", "10.0.0.0/08"]) expect(isCIDR(cidr)).toBe(false);
  });
});

 it("preserves ordered watch patterns and rejects traversal", () => {
  const git = { ...base, sourceKind: "git" as const, repository: "https://github.com/example/app", watchPaths: "src/**\n!**/*.test.ts\nsrc/keep.test.ts" };
  expect(validateWorkload(git, "edit").input?.source?.watch_paths).toEqual(["src/**", "!**/*.test.ts", "src/keep.test.ts"]);
  expect(validateWorkload({ ...git, watchPaths: "../secret" }, "edit").errors.watchPaths).toBeDefined();
 });

describe("route maintenance, error pages and forward-auth", () => {
  const extended: Route = {
    domain: "app.example.com",
    service: "web",
    port: 8080,
    policy: {
      maintenance: { enabled: false, html: "<h1>Back soon</h1>" },
      error_pages: { "404": "<h1>Not found</h1>", "502": "<h1>Unavailable</h1>" },
      forward_auth: { service: "auth", port: 9091, path: "/api/verify?rd=1", response_headers: ["Remote-User", "Remote-Groups"], response_cookies: ["session"] },
      certificate_source: "default",
    },
  };
  const edit = (route: Route, patch: Partial<PolicyDraft>): RouteRow => {
    const [row] = routeRows([route]) as [RouteRow];
    return { ...row, policy: { ...(row.policy ?? emptyPolicy()), ...patch } };
  };

  it("round-trips saved settings without adding removals", () => {
    const rows = routeRows([extended]);
    expect(sameRoutes(rows, [extended])).toBe(true);
    expect(validateRoutes(rows, [extended])).toEqual({ routes: [extended], errors: {} });
    const url: Route = { domain: "app.example.com", service: "web", port: 80, policy: { forward_auth: { url: "https://auth.example.com/api/verify" }, maintenance: { enabled: false } } };
    expect(validateRoutes(routeRows([url]), [url]).routes).toEqual([url]);
  });

  it("sends explicit nulls for every removed setting so the API does not keep them", () => {
    const cleared = edit(extended, { maintenance: null, errorPages: [], forwardAuth: emptyForwardAuth() });
    expect(sameRoutes([cleared], [extended])).toBe(false);
    expect(validateRoutes([cleared], [extended]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "default", maintenance: null, error_pages: null, forward_auth: null } }]);
    const onlyAuth = edit(extended, { forwardAuth: emptyForwardAuth() });
    expect(validateRoutes([onlyAuth], [extended]).routes?.[0]?.policy?.forward_auth).toBeNull();
    expect(validateRoutes([onlyAuth], [extended]).routes?.[0]?.policy?.maintenance).toEqual({ enabled: false, html: "<h1>Back soon</h1>" });
  });

  it("also clears settings when a route is deleted and added again on the same hostname", () => {
    const fresh: RouteRow = { id: "new", domain: "APP.example.com", service: "web", port: 8080 };
    expect(validateRoutes([fresh], [extended]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { maintenance: null, error_pages: null, forward_auth: null } }]);
    // Unrelated hostnames and new resources keep the legacy shape older workers accept.
    expect(validateRoutes([{ ...fresh, domain: "other.example.com" }], [extended]).routes).toEqual([{ domain: "other.example.com", service: "web", port: 8080 }]);
    expect(validateRoutes([fresh]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080 }]);
  });

  it("keeps a redacted external destination without asking for it again", () => {
    // Workload reads omit the verification URL; a saved forward-auth without URL or service is an external one.
    const redacted: Route = { domain: "app.example.com", service: "web", port: 80, policy: { forward_auth: { response_headers: ["Remote-User"] }, maintenance: { enabled: true } } };
    const rows = routeRows([redacted]);
    expect(rows[0]?.policy?.forwardAuth).toMatchObject({ mode: "url", url: "", hidden: true });
    expect(sameRoutes(rows, [redacted])).toBe(true);
    // An unrelated change saves without a URL, so the API keeps the saved destination.
    const [row] = rows as [RouteRow];
    const changed = { ...row, port: 8080 };
    expect(validateRoutes([changed], [redacted])).toEqual({ routes: [{ ...redacted, port: 8080 }], errors: {} });
    // A new URL replaces it; without a saved destination on the hostname, a blank URL is still an error.
    const replaced = edit(redacted, { forwardAuth: { ...row.policy!.forwardAuth, url: "https://auth.example.com/verify" } });
    expect(validateRoutes([replaced], [redacted]).routes?.[0]?.policy?.forward_auth).toEqual({ url: "https://auth.example.com/verify", response_headers: ["Remote-User"] });
    expect(validateRoutes(rows).errors[`${row.id}.auth.url`]).toBeDefined();
    expect(validateRoutes([{ ...row, domain: "other.example.com" }], [redacted]).errors[`${row.id}.auth.url`]).toBeDefined();
  });

  it("omits a redacted service path unless a new one is typed, and drops typed secrets after a save", () => {
    const redacted: Route = { domain: "app.example.com", service: "web", port: 80, policy: { forward_auth: { service: "auth", port: 9091 } } };
    const [row] = routeRows([redacted]) as [RouteRow];
    expect(row.policy?.forwardAuth).toMatchObject({ mode: "service", path: "", hidden: true });
    expect(validateRoutes([row], [redacted]).routes).toEqual([redacted]);
    const typed = edit(redacted, { forwardAuth: { ...row.policy!.forwardAuth, path: "/verify", url: "https://unused.example.com" } });
    expect(validateRoutes([typed], [redacted]).routes?.[0]?.policy?.forward_auth).toEqual({ service: "auth", port: 9091, path: "/verify" });
    const [cleared] = withoutAuthSecrets([typed]) as [RouteRow];
    expect(cleared.policy?.forwardAuth).toMatchObject({ mode: "service", service: "auth", url: "", path: "", hidden: true });
    expect(withoutAuthSecrets([{ id: "plain", domain: "a.example.com", service: "web", port: 80 }])).toEqual([{ id: "plain", domain: "a.example.com", service: "web", port: 80 }]);
  });

  it("saves only the selected forward-auth mode", () => {
    const [row] = routeRows([extended]) as [RouteRow];
    const auth = row.policy?.forwardAuth ?? emptyForwardAuth();
    const switched = edit(extended, { forwardAuth: { ...auth, mode: "url", url: " https://auth.example.com/verify " } });
    expect(validateRoutes([switched]).routes?.[0]?.policy?.forward_auth).toEqual({ url: "https://auth.example.com/verify", response_headers: ["Remote-User", "Remote-Groups"], response_cookies: ["session"] });
  });

  it("reports invalid settings next to their fields", () => {
    const auth = { ...emptyForwardAuth(), mode: "url" as const };
    const cases: [Partial<PolicyDraft>, string][] = [
      [{ forwardAuth: { ...auth, url: "http://auth.example.com" } }, "auth.url"],
      [{ forwardAuth: { ...auth, url: "https://user:pw@auth.example.com" } }, "auth.url"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com/#x" } }, "auth.url"],
      [{ forwardAuth: { ...auth, url: "https://10.0.0.5/verify" } }, "auth.url"],
      [{ forwardAuth: { ...auth, url: "https://[fd00::1]/verify" } }, "auth.url"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseHeaders: "X-Forwarded-User" } }, "auth.headers"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseHeaders: "Remote-User\nremote-user" } }, "auth.headers"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseHeaders: "Set-Cookie" } }, "auth.headers"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseHeaders: "Keep-Alive" } }, "auth.headers"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseHeaders: "X-Cove-Auth-Hop" } }, "auth.headers"],
      [{ requestHeaders: [{ id: "h", name: "Proxy-Connection", value: "x" }] }, "request.h"],
      [{ responseHeaders: [{ id: "h", name: "keep-alive", value: "x" }] }, "response.h"],
      [{ forwardAuth: { ...auth, url: "https://auth.example.com", responseCookies: "session\nsession" } }, "auth.cookies"],
      [{ forwardAuth: { ...auth, mode: "service", service: "auth", port: 70000 } }, "auth.service"],
      [{ forwardAuth: { ...auth, mode: "service", service: "auth", port: 9091, path: "//evil" } }, "auth.path"],
      [{ maintenance: { enabled: true, html: "é".repeat(8193) } }, "maintenance"],
      [{ errorPages: Array.from({ length: 9 }, (_, index) => ({ id: `p${index}`, status: String(500 + index), html: "x" })) }, "pages"],
      [{ errorPages: Array.from({ length: 5 }, (_, index) => ({ id: `p${index}`, status: String(500 + index), html: "x".repeat(16_000) })) }, "pages"],
    ];
    for (const [patch, key] of cases) {
      const result = validateRoutes([{ id: "r", domain: "app.example.com", service: "web", port: 80, policy: { ...emptyPolicy(), ...patch } }]);
      expect(result.errors, JSON.stringify(patch).slice(0, 80)).toHaveProperty(`r.${key}`);
    }
    const pages = validateRoutes([{ id: "r", domain: "app.example.com", service: "web", port: 80, policy: { ...emptyPolicy(), errorPages: [{ id: "a", status: "399", html: "x" }, { id: "b", status: "502", html: " " }, { id: "c", status: "404", html: "x" }, { id: "d", status: "404", html: "y" }] } }]);
    expect(Object.keys(pages.errors).sort()).toEqual(["r.page.a", "r.page.b", "r.page.d"]);
    const publicURL = validateRoutes([{ id: "r", domain: "app.example.com", service: "web", port: 80, policy: { ...emptyPolicy(), forwardAuth: { ...auth, url: "https://8.8.8.8/verify" } } }]);
    expect(publicURL.errors).toEqual({});
  });

  it("names active settings in summaries", () => {
    expect(extensionSummary(extended.policy)).toEqual(["forward-auth", "2 error pages"]);
    expect(extensionSummary({ maintenance: { enabled: true } })).toEqual(["maintenance on"]);
    expect(extensionSummary(undefined)).toEqual([]);
  });
});
