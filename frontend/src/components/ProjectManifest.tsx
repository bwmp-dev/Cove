import type { UseQueryResult } from "@tanstack/react-query";
import { useMemo } from "react";
import type { ProjectConfiguration } from "../api/project-configuration";
import { countLabel } from "../lib/hierarchy";
import { manifestYaml } from "../lib/project-configuration";
import { CopyButton } from "./CopyButton";
import { DefinitionList, LoadingRows, QueryError, Section } from "./ui";

const description = "The project's declaration in cove.yaml form. Variable values, source tokens, deploy keys, infrastructure credentials and private file contents are left out; variables are listed by name.";

export function ProjectManifest({ configuration }: { configuration: UseQueryResult<ProjectConfiguration> }) {
  const yaml = useMemo(() => (configuration.data ? manifestYaml(configuration.data.manifest) : ""), [configuration.data]);
  if (configuration.isPending) {
    return (
      <Section title="Current configuration" description={description} flush>
        <LoadingRows label="Loading configuration" rows={3} />
      </Section>
    );
  }
  if (configuration.isError) {
    return (
      <Section title="Current configuration" description={description}>
        <QueryError what="the configuration" error={configuration.error} onRetry={() => void configuration.refetch()} retrying={configuration.isFetching} />
      </Section>
    );
  }
  const { manifest } = configuration.data;
  return (
    <Section title="Current configuration" description={description} actions={<CopyButton text={yaml} label="Copy cove.yaml" />}>
      <div className="space-y-4">
        <DefinitionList
          compact
          items={[
            { term: "Environments", value: countLabel((manifest.environments ?? []).length, "environment") },
            { term: "Resources", value: countLabel((manifest.resources ?? []).length, "resource") },
            { term: "Schema", value: <span className="font-mono text-xs">{manifest.api_version}</span> },
          ]}
        />
        <pre role="region" aria-label="Redacted cove.yaml" tabIndex={0} className="max-h-96 overflow-auto rounded-md border border-line bg-panel p-3 font-mono text-xs leading-relaxed text-fg">
          {yaml}
        </pre>
      </div>
    </Section>
  );
}
