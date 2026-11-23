import { describe, expect, it } from "vitest";
import { templateEntry } from "../test/templates";
import { draftRequest, initialValues, sourceURLError, suggestTemplateId, validateDraft, variableError } from "./templates";

const entry = templateEntry({
  variables: [
    { key: "DB_PASSWORD", label: "Database password", type: "secret", default: "", required: true, generated: true },
    { key: "ADMIN_EMAIL", label: "Admin email", type: "email", default: "", required: true, generated: false },
    { key: "WORKERS", label: "Workers", type: "integer", default: "2", required: false, generated: false },
  ],
  web: { route: true, service: "app", port: 8080, url_variable: "APP_URL" },
});

describe("template drafts", () => {
  it("omits unedited generated secrets so the server generates them, and sends edited values", () => {
    const values = initialValues(entry.template);
    expect(values).toEqual({ DB_PASSWORD: "", ADMIN_EMAIL: "", WORKERS: "2" });
    const request = draftRequest(entry, { ...values, ADMIN_EMAIL: "admin@example.com" }, " App.Example.com ", false);
    expect(request.values).toEqual({ ADMIN_EMAIL: "admin@example.com", WORKERS: "2" });
    expect(request).toMatchObject({ origin: "builtin", repository_id: "", id: "example", revision: "rev-1", hostname: "app.example.com" });
    const replaced = draftRequest(entry, { ...values, DB_PASSWORD: "a".repeat(64) }, "app.example.com", false);
    expect(replaced.values.DB_PASSWORD).toBe("a".repeat(64));
  });

  it("does not send a hostname for internal web hints", () => {
    const internal = templateEntry({ web: { route: false, service: "app", port: 8080, url_variable: "" } });
    expect(draftRequest(internal, {}, "ignored.example.com", false).hostname).toBe("");
    expect(validateDraft(internal, {}, "", false)).toEqual({});
  });

  it("checks typed values, replaced generated secrets, routes and host access acknowledgement", () => {
    const [secret, email, integer] = entry.template.variables;
    if (!secret || !email || !integer) throw new Error("fixture");
    expect(variableError(secret, "")).toBeNull();
    expect(variableError(secret, "not-hex")).toMatch(/64 lowercase hexadecimal/);
    expect(variableError(email, "")).toBe("Enter a value.");
    expect(variableError(email, "nope")).toMatch(/email/);
    expect(variableError(integer, "")).toBeNull();
    expect(variableError(integer, "1.5")).toMatch(/whole number/);
    const errors = validateDraft(entry, initialValues(entry.template), "https://app.example.com", false);
    expect(errors.ADMIN_EMAIL).toBe("Enter a value.");
    expect(errors.hostname).toMatch(/without a scheme/);
    const socket = templateEntry({ capabilities: ["docker_socket"] });
    expect(validateDraft(socket, {}, "", false)).toHaveProperty("acknowledge");
    expect(validateDraft(socket, {}, "", true)).toEqual({});
  });
});

describe("template sources", () => {
  it("accepts public HTTPS sources only", () => {
    expect(sourceURLError("https://github.com/example/templates.git")).toBeNull();
    expect(sourceURLError("https://github.com:443/example/templates.git")).toBeNull();
    expect(sourceURLError("http://github.com/example/templates.git")).toMatch(/HTTPS/);
    expect(sourceURLError("https://user:token@github.com/example/templates.git")).toMatch(/credentials/);
    expect(sourceURLError("https://example.com/index.json?token=1")).toMatch(/query string/);
    expect(sourceURLError("https://example.com:8443/index.json")).toMatch(/default HTTPS port/);
  });

  it("suggests a valid template ID from a resource name", () => {
    expect(suggestTemplateId("My App (prod)")).toBe("my-app-prod");
  });
});
