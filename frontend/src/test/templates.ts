import type { CatalogueTemplate, TemplateEntry } from "../api/templates";

// Minimal catalogue entries for component tests; the browser acceptance test uses the real server catalogue.
export function templateEntry(overrides: Partial<CatalogueTemplate> = {}, entry: Partial<Omit<TemplateEntry, "template">> = {}): TemplateEntry {
  return {
    origin: "builtin",
    repository_id: "",
    repository_name: "",
    repository_url: "",
    revision: "rev-1",
    warnings: [],
    validation: { compose_sha256: "abc", schema_validated: true, compose_config: true, runtime: "started_healthy", checked_at: "2026-11-22T17:40:17Z", message: "Started on an internal network.", services: { app: "healthy" } },
    ...entry,
    template: {
      schema_version: 1,
      id: "example",
      name: "Example",
      description: "Example application.",
      category: "Applications",
      source: "https://example.com/source",
      license: "https://example.com/license",
      docs: "https://example.com/docs",
      notes: "",
      images: { app: "example/app:1.2.3" },
      compose: "services:\n  app:\n    image: example/app:1.2.3\n",
      variables: [],
      web: null,
      storage: ["data"],
      backup: "Back up the data volume.",
      minimum: { cpus: 0.25, memory_mb: 256, disk_mb: 1024 },
      capabilities: [],
      file_secrets: null,
      ...overrides,
    },
  };
}
