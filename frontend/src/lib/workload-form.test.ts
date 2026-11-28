import { describe, expect, it } from "vitest";
import type { Listener } from "../api/ingress-listeners";
import type { Route } from "../api/types";
import { certificateSwitches, emptyForwardAuth, routesNeedProtocol4, emptyPolicy, extensionSummary, isCIDR, parseDotenv, refreshCertificateErrors, routeRows, sameRoutes, validateComposePath, validateHostKey, validateRepository, validateRoutes, validateWorkload, withoutRouteSecrets, redactedHeader, type HeaderRow, type PolicyDraft, type RouteRow, type WorkloadFormValues } from "./workload-form";

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
      row("wildcard", "*.wild.example.com"),
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
    expect(validateRoutes([fresh], [extended]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { maintenance: null, error_pages: null, forward_auth: null, certificate_id: null } }]);
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
    const [cleared] = withoutRouteSecrets([typed]) as [RouteRow];
    expect(cleared.policy?.forwardAuth).toMatchObject({ mode: "service", service: "auth", url: "", path: "", hidden: true });
    expect(withoutRouteSecrets([{ id: "plain", domain: "a.example.com", service: "web", port: 80 }])).toEqual([{ id: "plain", domain: "a.example.com", service: "web", port: 80 }]);
  });

  it("keeps hidden header values by sending the marker back until a value is typed or emptied", () => {
    const redacted: Route = { domain: "app.example.com", service: "web", port: 80, policy: { request_headers: { "X-Token": redactedHeader, "X-Strip": "" }, response_headers: { "X-Version": redactedHeader } } };
    const [row] = routeRows([redacted]) as [RouteRow];
    const token = row.policy!.requestHeaders.find((item) => item.name === "X-Token")!;
    expect(token).toMatchObject({ value: "", hidden: true });
    expect(row.policy!.requestHeaders.find((item) => item.name === "X-Strip")).toMatchObject({ value: "" });
    expect(row.policy!.requestHeaders.find((item) => item.name === "X-Strip")?.hidden).toBeUndefined();
    expect(sameRoutes([row], [redacted])).toBe(true);
    // Unchanged, the marker goes back so the API keeps the saved value; an explicit empty value stays empty.
    expect(validateRoutes([{ ...row, port: 8080 }], [redacted])).toEqual({ routes: [{ ...redacted, port: 8080 }], errors: {} });
    const headers = (requestHeaders: HeaderRow[]): RouteRow => ({ ...row, policy: { ...row.policy!, requestHeaders } });
    const others = row.policy!.requestHeaders.filter((item) => item !== token);
    expect(validateRoutes([headers([{ ...token, value: "new-secret" }, ...others])], [redacted]).routes?.[0]?.policy?.request_headers).toEqual({ "X-Token": "new-secret", "X-Strip": "" });
    expect(validateRoutes([headers([{ ...token, hidden: false }, ...others])], [redacted]).routes?.[0]?.policy?.request_headers).toEqual({ "X-Token": "", "X-Strip": "" });
    // The marker is matched to the same header (any case) on the same hostname; anything else needs a new value.
    expect(validateRoutes([headers([{ ...token, name: "x-token" }, ...others])], [redacted]).errors).toEqual({});
    expect(validateRoutes([headers([{ ...token, name: "X-Other" }, ...others])], [redacted]).errors[`${row.id}.request.${token.id}`]).toMatch(/Enter a new value/);
    expect(validateRoutes([{ ...row, domain: "other.example.com" }], [redacted]).errors[`${row.id}.request.${token.id}`]).toMatch(/Enter a new value/);
    expect(validateRoutes([headers([{ ...token, value: redactedHeader }, ...others])], [redacted]).errors[`${row.id}.request.${token.id}`]).toMatch(/reserved/);
  });

  it("sends removed header maps as null so the API does not keep them", () => {
    const redacted: Route = { domain: "app.example.com", service: "web", port: 80, policy: { request_headers: { "X-Token": redactedHeader }, response_headers: { "X-Version": redactedHeader } } };
    const cleared = edit(redacted, { requestHeaders: [] });
    expect(validateRoutes([cleared], [redacted]).routes?.[0]?.policy).toEqual({ request_headers: null, response_headers: { "X-Version": redactedHeader } });
  });

  it("drops typed header values after a save and keeps explicit empty values", () => {
    const [row] = routeRows([{ domain: "app.example.com", service: "web", port: 80 }]) as [RouteRow];
    const typed: RouteRow = { ...row, policy: { ...emptyPolicy(), requestHeaders: [{ id: "a", name: "X-Token", value: "secret" }, { id: "b", name: "X-Strip", value: "" }] } };
    const [after] = withoutRouteSecrets([typed]) as [RouteRow];
    expect(after.policy?.requestHeaders).toEqual([{ id: "a", name: "X-Token", value: "", hidden: true }, { id: "b", name: "X-Strip", value: "" }]);
    expect(validateRoutes([after], [{ domain: "app.example.com", service: "web", port: 80, policy: { request_headers: { "X-Token": redactedHeader, "X-Strip": "" } } }]).routes?.[0]?.policy?.request_headers).toEqual({ "X-Token": redactedHeader, "X-Strip": "" });
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

describe("uploaded route certificates", () => {
  const id = "a".repeat(32);
  const other = "b".repeat(32);
  const uploaded: Route = { domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: id } };
  const metadata = (certificateId: string, dnsNames: string[]) => ({ id: certificateId, name: "Origin", fingerprint: "f".repeat(64), dns_names: dnsNames, not_before: "2026-01-01T00:00:00Z", expires_at: "2027-01-01T00:00:00Z", created_at: "2026-01-01T00:00:00Z" });
  const withPolicy = (route: Route, patch: Partial<PolicyDraft>): RouteRow => {
    const [row] = routeRows([route]) as [RouteRow];
    return { ...row, policy: { ...(row.policy ?? emptyPolicy()), ...patch } };
  };

  it("round-trips a saved selection, even when the certificate list is unavailable", () => {
    const rows = routeRows([uploaded]);
    expect(rows[0]?.policy).toMatchObject({ certificateSource: "uploaded", certificateId: id });
    expect(sameRoutes(rows, [uploaded])).toBe(true);
    expect(validateRoutes(rows, [uploaded])).toEqual({ routes: [uploaded], errors: {} });
    expect(validateRoutes(rows, [uploaded], [metadata(other, ["app.example.com"])])).toEqual({ routes: [uploaded], errors: {} });
  });

  it("names the new source when switching away so the saved selection is not kept", () => {
    const acme = withPolicy(uploaded, { certificateSource: "acme", certificateId: "" });
    expect(validateRoutes([acme], [uploaded]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "acme" } }]);
    const fallback = withPolicy(uploaded, { certificateSource: "default", certificateId: "" });
    expect(validateRoutes([fallback], [uploaded]).routes?.[0]?.policy).toEqual({ certificate_source: "default" });
  });

  it("clears the selection with a null ID when the hostname is added again without a certificate source", () => {
    const fresh: RouteRow = { id: "new", domain: "app.example.com", service: "web", port: 8080 };
    expect(validateRoutes([fresh], [uploaded]).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_id: null } }]);
    expect(validateRoutes([{ ...fresh, domain: "new.example.com" }], [uploaded]).routes).toEqual([{ domain: "new.example.com", service: "web", port: 8080 }]);
  });

  it("requires a chosen certificate and the same certificate on every path of a hostname", () => {
    const unchosen = withPolicy(uploaded, { certificateId: "" });
    expect(validateRoutes([unchosen]).errors[`${unchosen.id}.certificate`]).toBe("Choose a certificate version.");
    const api = withPolicy({ ...uploaded, policy: { path_prefix: "/api" } }, { certificateSource: "acme" });
    expect(validateRoutes([...routeRows([uploaded]), api]).errors[`${api.id}.certificate`]).toMatch(/same certificate source and version/);
    const apiSame = withPolicy({ ...uploaded, policy: { path_prefix: "/api" } }, { certificateSource: "uploaded", certificateId: id });
    expect(validateRoutes([...routeRows([uploaded]), apiSame]).errors).toEqual({});
  });

  it("lists hostnames that start or stop using an uploaded certificate", () => {
    expect(certificateSwitches(routeRows([uploaded]), [uploaded])).toEqual([]);
    expect(certificateSwitches([withPolicy(uploaded, { certificateId: other })], [uploaded])).toEqual([]);
    expect(certificateSwitches([withPolicy(uploaded, { certificateSource: "acme", certificateId: "" })], [uploaded])).toEqual(["app.example.com"]);
    expect(certificateSwitches([], [uploaded])).toEqual(["app.example.com"]);
    expect(certificateSwitches(routeRows([uploaded]), [])).toEqual(["app.example.com"]);
  });

  it("reports certificates naming hostnames outside the resource's routes", () => {
    const rows = routeRows([uploaded]);
    expect(validateRoutes(rows, [], [metadata(id, ["app.example.com", "api.example.com"])]).errors[`${rows[0]?.id}.certificate`]).toMatch(/api\.example\.com/);
    const both = [...rows, { id: "api", domain: "api.example.com", service: "web", port: 8080 }];
    expect(validateRoutes(both, [], [metadata(id, ["app.example.com", "api.example.com"])]).errors).toEqual({});
  });

  it("re-checks shown certificate errors against the current rows and keeps other errors", () => {
    const certificates = [metadata(id, ["app.example.com", "api.example.com"]), metadata(other, ["app.example.com"])];
    const row = withPolicy(uploaded, {});
    const wide = [row];
    const key = `${row.id}.certificate`;
    const shown = { ...validateRoutes(wide, [], certificates).errors, [`${row.id}.path`]: "Start with /." };
    expect(shown[key]).toMatch(/api\.example\.com/);
    const choose = (patch: Partial<PolicyDraft>): RouteRow[] => [{ ...row, policy: { ...emptyPolicy(), ...row.policy, ...patch } }];
    expect(refreshCertificateErrors(shown, choose({ certificateId: other }), [], certificates)).toEqual({ [`${row.id}.path`]: "Start with /." });
    const owned = [...wide, { id: "api", domain: "api.example.com", service: "web", port: 8080 }];
    expect(refreshCertificateErrors(shown, owned, [], certificates)[key]).toBeUndefined();
    expect(refreshCertificateErrors(shown, wide, [], certificates)[key]).toBe(shown[key]);
    expect(refreshCertificateErrors(shown, choose({ certificateId: "" }), [], certificates)[key]).toBe("Choose a certificate version.");
    expect(refreshCertificateErrors({}, wide, [], certificates)).toEqual({});
  });
});

describe("wildcard routes and route protocol 4", () => {
  const id = "a".repeat(32);
  const metadata = (certificateId: string, dnsNames: string[]) => ({ id: certificateId, name: "Wildcard", fingerprint: "f".repeat(64), dns_names: dnsNames, not_before: "2026-01-01T00:00:00Z", expires_at: "2027-01-01T00:00:00Z", created_at: "2026-01-01T00:00:00Z" });
  const wildcard: Route = { domain: "*.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: id } };
  const proxyDefault: Route = { domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "default" } };

  it("accepts a wildcard route that selects a certificate naming the same wildcard", () => {
    expect(validateRoutes(routeRows([wildcard]), [], [metadata(id, ["*.example.com"])], 4)).toEqual({ routes: [wildcard], errors: {} });
  });

  it("refuses incomplete wildcards and wildcards without a stored certificate", () => {
    const [bare] = routeRows([{ domain: "*.com", service: "web", port: 8080 }]) as [RouteRow];
    expect(validateRoutes([bare], [], [], 4).errors[bare.id]).toMatch(/DNS hostname/);
    const [plain] = routeRows([{ domain: "*.example.com", service: "web", port: 8080 }]) as [RouteRow];
    expect(validateRoutes([plain], [], [], 4).errors[plain.id]).toMatch(/stored certificate .*Advanced routing/);
    const [acme] = routeRows([{ ...wildcard, policy: { certificate_source: "acme" } }]) as [RouteRow];
    expect(validateRoutes([acme], [], [], 4).errors[`${acme.id}.certificate`]).toMatch(/Let's Encrypt cannot issue one/);
  });

  it("requires the exact wildcard name, not only names below it", () => {
    const rows = routeRows([wildcard]);
    expect(validateRoutes(rows, [], [metadata(id, ["app.example.com"])], 4).errors[`${rows[0]?.id}.certificate`]).toMatch(/names \*\.example\.com itself/);
  });

  it("refuses new wildcard and proxy default choices on an older worker but keeps saved ones for the server to judge", () => {
    const apexDefault: Route = { ...proxyDefault, domain: "example.com" };
    const rows = routeRows([wildcard, apexDefault]);
    const fresh = validateRoutes(rows, [], [metadata(id, ["*.example.com"])], 3).errors;
    expect(fresh[rows[0]?.id ?? ""]).toMatch(/newer worker/);
    expect(fresh[`${rows[1]?.id}.certificate`]).toMatch(/newer worker/);
    expect(validateRoutes(rows, [wildcard, apexDefault], [metadata(id, ["*.example.com"])], 3).errors).toEqual({});
    expect(validateRoutes(rows, [], [metadata(id, ["*.example.com"])], undefined).errors).toEqual({});
    expect(routesNeedProtocol4(rows)).toBe(true);
    expect(routesNeedProtocol4(routeRows([{ domain: "app.example.com", service: "web", port: 8080 }]))).toBe(false);
  });

  it("counts switches between Let's Encrypt and the proxy default as proxy restarts, but not stored to default", () => {
    const stored: Route = { domain: "app.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: id } };
    const plain: Route = { domain: "app.example.com", service: "web", port: 8080 };
    expect(certificateSwitches(routeRows([proxyDefault]), [plain])).toEqual(["app.example.com"]);
    expect(certificateSwitches(routeRows([plain]), [proxyDefault])).toEqual(["app.example.com"]);
    expect(certificateSwitches(routeRows([proxyDefault]), [stored])).toEqual([]);
    expect(certificateSwitches(routeRows([stored]), [proxyDefault])).toEqual([]);
  });
});

describe("one certificate selection per hostname", () => {
  const id = "a".repeat(32);
  const route = (path: string, policy: Route["policy"]): Route => ({ domain: "app.example.com", service: "web", port: 8080, policy: { ...(path ? { path_prefix: path } : {}), ...policy } });

  it("refuses mixing the proxy default with Lets Encrypt, or different stored versions, on one hostname", () => {
    const mixed = routeRows([route("", { certificate_source: "default" }), route("/api", { certificate_source: "acme" })]);
    expect(validateRoutes(mixed, [], [], 4).errors[`${mixed[1]?.id}.certificate`]).toMatch(/same certificate source and version/);
    const omitted = routeRows([route("/api", { certificate_source: "default" }), { domain: "app.example.com", service: "web", port: 8080 }]);
    expect(validateRoutes(omitted, [], [], 4).errors[omitted[1]?.id ?? ""]).toMatch(/same certificate source and version.*Advanced routing/);
    const versions = routeRows([route("", { certificate_source: "uploaded", certificate_id: id }), route("/api", { certificate_source: "uploaded", certificate_id: "b".repeat(32) })]);
    expect(validateRoutes(versions, [], [], 4).errors[`${versions[1]?.id}.certificate`]).toMatch(/same certificate source and version/);
  });

  it("treats an omitted source as Lets Encrypt and accepts matching selections", () => {
    expect(validateRoutes(routeRows([route("", {}), route("/api", { certificate_source: "acme" })]), [], [], 4).errors).toEqual({});
    expect(validateRoutes(routeRows([route("", { certificate_source: "default" }), route("/api", { certificate_source: "default" })]), [], [], 4).errors).toEqual({});
  });

  it("keeps a saved proxy default explicit and clears it explicitly when the hostname is added again without one", () => {
    const saved = route("", { certificate_source: "default" });
    expect(validateRoutes(routeRows([saved]), [saved], [], 4).routes).toEqual([saved]);
    const fresh: RouteRow = { id: "new", domain: "app.example.com", service: "web", port: 8080 };
    expect(validateRoutes([fresh], [saved], [], 4).routes).toEqual([{ domain: "app.example.com", service: "web", port: 8080, policy: { certificate_id: null } }]);
  });
});

describe("exact hostnames below a wildcard route", () => {
  const wildcardId = "a".repeat(32);
  const exactId = "b".repeat(32);
  const wildcard: Route = { domain: "*.example.com", service: "web", port: 8080, policy: { certificate_source: "uploaded", certificate_id: wildcardId } };
  const exact = (policy?: Route["policy"], domain = "app.example.com"): Route => ({ domain, service: "web", port: 8080, ...(policy ? { policy } : {}) });

  it("requires a stored certificate on a one-label child of a wildcard route", () => {
    for (const policy of [{ certificate_source: "acme" as const }, { certificate_source: "default" as const }]) {
      const rows = routeRows([wildcard, exact(policy)]);
      expect(validateRoutes(rows, [], [], 4).errors[`${rows[1]?.id}.certificate`]).toMatch(/covered by the wildcard route \*\.example\.com/);
    }
    const plain = routeRows([wildcard, exact()]);
    expect(validateRoutes(plain, [], [], 4).errors[plain[1]?.id ?? ""]).toMatch(/covered by the wildcard route.*Advanced routing/);
  });

  it("accepts the wildcard version or another stored version on the child, and leaves the apex and deeper names alone", () => {
    expect(validateRoutes(routeRows([wildcard, exact({ certificate_source: "uploaded", certificate_id: wildcardId })]), [], [], 4).errors).toEqual({});
    expect(validateRoutes(routeRows([wildcard, exact({ certificate_source: "uploaded", certificate_id: exactId })]), [], [], 4).errors).toEqual({});
    expect(validateRoutes(routeRows([wildcard, exact(undefined, "example.com"), exact({ certificate_source: "acme" }, "a.b.example.com")]), [], [], 4).errors).toEqual({});
  });
});

describe("private route visibility", () => {
  // Fixture listeners shaped like GET /api/workloads/{id}/route-listeners; not real node configuration.
  const listeners: Listener[] = [
    { id: "office", protocol: "https", visibility: "lan", bindings: [{ address: "192.168.10.2", port: 8443 }] },
    { id: "tailnet", protocol: "https", visibility: "tailscale", bindings: [{ address: "100.100.1.2", port: 8443 }] },
    { id: "raw", protocol: "tcp", visibility: "lan", bindings: [{ address: "192.168.10.2", port: 5432 }] },
  ];
  const privateRoute = (policy: Route["policy"] = {}, domain = "app.example.com"): Route => ({ domain, service: "web", port: 8080, policy: { visibility: "lan", listener: "office", certificate_source: "default", ...policy } });
  const publicRoute: Route = { domain: "app.example.com", service: "web", port: 8080 };

  it("keeps legacy public routes in their old shape", () => {
    expect(validateRoutes(routeRows([publicRoute]), [publicRoute], [], 4, listeners)).toEqual({ routes: [publicRoute], errors: {} });
  });

  it("round-trips a saved private route unchanged and keeps it clean", () => {
    const saved = privateRoute();
    expect(sameRoutes(routeRows([saved]), [saved])).toBe(true);
    expect(validateRoutes(routeRows([saved]), [saved], [], 5, listeners)).toEqual({ routes: [saved], errors: {} });
  });

  it("serializes a new private route with its listener and certificate", () => {
    const rows = routeRows([publicRoute]);
    const row = rows[0] as RouteRow;
    row.policy = { ...emptyPolicy(), visibility: "tailscale", listener: "tailnet", certificateSource: "default" };
    expect(validateRoutes(rows, [publicRoute], [], 5, listeners).routes).toEqual([{ ...publicRoute, policy: { visibility: "tailscale", listener: "tailnet", certificate_source: "default" } }]);
  });

  it("switches a saved private hostname to public with an explicit public visibility and null listener", () => {
    const saved = privateRoute();
    const rows = routeRows([saved]);
    const row = rows[0] as RouteRow;
    row.policy = { ...(row.policy as PolicyDraft), visibility: "", listener: "" };
    expect(validateRoutes(rows, [saved], [], 5, listeners).routes).toEqual([{ ...publicRoute, policy: { certificate_source: "default", visibility: "public", listener: null } }]);
    const fresh: RouteRow = { id: "new", domain: "app.example.com", service: "web", port: 8080 };
    expect(validateRoutes([fresh], [saved], [], 5, listeners).routes).toEqual([{ ...publicRoute, policy: { certificate_id: null, visibility: "public", listener: null } }]);
  });

  it("keeps an explicitly saved public visibility", () => {
    const saved: Route = { ...publicRoute, policy: { visibility: "public" } };
    expect(sameRoutes(routeRows([saved]), [saved])).toBe(true);
    expect(validateRoutes(routeRows([saved]), [saved], [], 5, listeners).routes).toEqual([saved]);
  });

  it("requires a listener of the same visibility and HTTPS protocol", () => {
    const missing = routeRows([privateRoute({ listener: "" })]);
    expect(validateRoutes(missing, [], [], 5, listeners).errors[`${missing[0]?.id}.listener`]).toBe("Choose the listener that serves this hostname.");
    const wrong = routeRows([privateRoute({ listener: "tailnet" })]);
    expect(validateRoutes(wrong, [], [], 5, listeners).errors[`${wrong[0]?.id}.listener`]).toMatch(/not a LAN HTTPS listener/);
    const tcp = routeRows([privateRoute({ listener: "raw" })]);
    expect(validateRoutes(tcp, [], [], 5, listeners).errors[`${tcp[0]?.id}.listener`]).toMatch(/not a LAN HTTPS listener/);
  });

  it("keeps a stored listener that is no longer listed, and leaves it to the API while the list is unknown", () => {
    const saved = privateRoute({ listener: "removed" });
    const rows = routeRows([saved]);
    expect(validateRoutes(rows, [saved], [], 5, listeners).errors[`${rows[0]?.id}.listener`]).toMatch(/Listener removed is not configured/);
    expect(validateRoutes(rows, [saved], [], 5, undefined)).toEqual({ routes: [saved], errors: {} });
    expect(rows[0]?.policy?.listener).toBe("removed");
  });

  it("rejects Let's Encrypt on a private route", () => {
    for (const policy of [{ certificate_source: "acme" as const }, { certificate_source: undefined }]) {
      const rows = routeRows([privateRoute(policy)]);
      expect(validateRoutes(rows, [], [], 5, listeners).errors[`${rows[0]?.id}.certificate`]).toMatch(/Private routes cannot use Let's Encrypt/);
    }
  });

  it("requires the same visibility on every path and overlapping wildcard", () => {
    const paths = routeRows([privateRoute(), { ...publicRoute, policy: { path_prefix: "/api", certificate_source: "default" } }]);
    expect(validateRoutes(paths, [], [], 5, listeners).errors[`${paths[1]?.id}.visibility`]).toBe("Every path on app.example.com must use the same visibility and listener.");
    const wildcard = routeRows([privateRoute({ certificate_source: "uploaded", certificate_id: "a".repeat(32) }, "*.example.com"), { ...publicRoute, policy: { certificate_source: "uploaded", certificate_id: "a".repeat(32) } }]);
    expect(validateRoutes(wildcard, [], [], 5, listeners).errors[`${wildcard[1]?.id}.visibility`]).toMatch(/app\.example\.com overlaps \*\.example\.com/);
  });

  it("rejects new privacy on an older worker but keeps saved privacy for the control plane", () => {
    const rows = routeRows([publicRoute]);
    const row = rows[0] as RouteRow;
    row.policy = { ...emptyPolicy(), visibility: "lan", listener: "office", certificateSource: "default" };
    expect(validateRoutes(rows, [publicRoute], [], 4).errors[`${row.id}.visibility`]).toMatch(/needs a newer worker/);
    const saved = privateRoute();
    expect(validateRoutes(routeRows([saved]), [saved], [], 4)).toEqual({ routes: [saved], errors: {} });
  });

  it("clears a shown listener error once another listener is chosen", () => {
    const rows = routeRows([privateRoute({ listener: "" })]);
    const shown = validateRoutes(rows, [], [], 5, listeners).errors;
    (rows[0]?.policy as PolicyDraft).listener = "office";
    expect(refreshCertificateErrors(shown, rows, [], [], 5, listeners)).toEqual({});
  });
});

describe("HTTP upstream timeouts", () => {
  const timed: Route = { domain: "app.example.com", service: "web", port: 80, policy: { timeouts: { dial_seconds: 5, idle_connection_seconds: 120 } } };
  const plainRoute: Route = { domain: "app.example.com", service: "web", port: 80 };
  const policyOf = (rows: RouteRow[]) => rows[0]?.policy as PolicyDraft;

  it("keeps legacy routes without timeouts in their legacy shape", () => {
    const rows = routeRows([plainRoute]);
    expect(sameRoutes(rows, [plainRoute])).toBe(true);
    expect(validateRoutes(rows, [plainRoute], [], 6).routes).toEqual([plainRoute]);
  });

  it("keeps an explicit http protocol unchanged and saves it back", () => {
    const explicit: Route = { ...plainRoute, protocol: "http" };
    const rows = routeRows([explicit]);
    expect(sameRoutes(rows, [explicit])).toBe(true);
    expect(validateRoutes(rows, [explicit], [], 6).routes).toEqual([explicit]);
    expect(routeRows([plainRoute])[0]).not.toHaveProperty("protocol");
  });

  it("round-trips saved timeouts and sends only positive overrides", () => {
    const rows = routeRows([timed]);
    expect(sameRoutes(rows, [timed])).toBe(true);
    expect(validateRoutes(rows, [timed], [], 6).routes).toEqual([timed]);
    policyOf(rows).timeouts = { dial: "0", header: "30", idle: "" };
    expect(validateRoutes(rows, [timed], [], 6).routes).toEqual([{ ...plainRoute, policy: { timeouts: { response_header_seconds: 30 } } }]);
  });

  it("clears saved timeouts with an explicit null, also on a route removed and added again", () => {
    const rows = routeRows([timed]);
    policyOf(rows).timeouts = { dial: "", header: "", idle: "0" };
    expect(validateRoutes(rows, [timed], [], 6).routes).toEqual([{ ...plainRoute, policy: { timeouts: null } }]);
    const readded: RouteRow[] = [{ id: "r2", domain: "app.example.com", service: "web", port: 80 }];
    expect(validateRoutes(readded, [timed], [], 6).routes).toEqual([{ ...plainRoute, policy: { timeouts: null } }]);
  });

  it("validates whole seconds within the worker's limits", () => {
    const rows = routeRows([plainRoute]);
    const id = rows[0]?.id ?? "";
    rows[0] = { ...rows[0]!, policy: { ...emptyPolicy(), timeouts: { dial: "601", header: "1.5", idle: "3601" } } };
    expect(validateRoutes(rows, [], [], 6).errors).toEqual({ [`${id}.timeouts.dial`]: "Enter whole seconds from 0 to 600.", [`${id}.timeouts.header`]: "Enter whole seconds from 0 to 600.", [`${id}.timeouts.idle`]: "Enter whole seconds from 0 to 3600." });
    rows[0] = { ...rows[0], policy: { ...emptyPolicy(), timeouts: { dial: "600", header: "600", idle: "3600" } } };
    expect(validateRoutes(rows, [], [], 6).errors).toEqual({});
  });

  it.each([
    [5, /need a newer worker/],
    [undefined, /once the worker capabilities of the node are known/],
  ])("rejects new timeouts on route protocol %s but keeps saved ones and allows clearing them", (protocol, message) => {
    const added = routeRows([plainRoute]);
    added[0] = { ...added[0]!, policy: { ...emptyPolicy(), timeouts: { dial: "5", header: "", idle: "" } } };
    expect(validateRoutes(added, [plainRoute], [], protocol).errors[`${added[0].id}.timeouts`]).toMatch(message);
    expect(validateRoutes(routeRows([timed]), [timed], [], protocol).errors).toEqual({});
    const cleared = routeRows([timed]);
    policyOf(cleared).timeouts = { dial: "", header: "", idle: "" };
    expect(validateRoutes(cleared, [timed], [], protocol).routes).toEqual([{ ...plainRoute, policy: { timeouts: null } }]);
  });
});
