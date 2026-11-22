import { useQuery } from "@tanstack/react-query";
import { gitOpsQuery, projectConfigurationQuery } from "../api/project-configuration";
import { ConfigurationDrafts } from "../components/ConfigurationDrafts";
import { ConfigurationExport } from "../components/ConfigurationExport";
import { CoolifyImport } from "../components/CoolifyImport";
import { EnvironmentCompare } from "../components/EnvironmentCompare";
import { GitOpsSource } from "../components/GitOpsSource";
import { ProjectManifest } from "../components/ProjectManifest";
import { ProjectStatusShare } from "../components/ProjectStatusShare";
import { EmptyState, Notice, SectionGrid } from "../components/ui";
import { canCompareEnvironments, productionEnvironments } from "../lib/hierarchy";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { useSession } from "../lib/session";
import { useProject } from "./project";

export function ProjectConfiguration() {
  const { project, resources } = useProject();
  const online = useOnline();
  const session = useSession();
  const readable = can(project.permissions, "read");
  const editable = can(project.permissions, "edit");
  const configuration = useQuery({ ...projectConfigurationQuery(project.id), enabled: readable });
  const gitops = useQuery({ ...gitOpsQuery(project.id), enabled: readable });
  const environments = productionEnvironments(project);

  if (!readable && canCompareEnvironments(project)) {
    return (
      <>
        <div className="px-4 pt-5 sm:px-6">
          <Notice>You can compare the environments you can read. The project's configuration, drafts, GitOps and export need read access to the project.</Notice>
        </div>
        <EnvironmentCompare projectId={project.id} environments={environments} />
      </>
    );
  }
  if (!readable) {
    return <EmptyState title="Configuration needs read access">Ask a team owner or admin for access to this project.</EmptyState>;
  }
  return (
    <>
      {(!online || !editable) && (
        <div className="space-y-2 px-4 pt-5 sm:px-6">
          {!online && <Notice tone="warn" title="You are offline">Changes are unavailable until the connection returns.</Notice>}
          {!editable && <Notice>You can view this configuration. Saving drafts, changing GitOps settings and exporting need editor access to the project.</Notice>}
        </div>
      )}
      <ProjectManifest configuration={configuration} />
      {editable && <ConfigurationDrafts projectId={project.id} currentVersion={configuration.data?.version} online={online} gitOpsDraft={gitops.data?.connected ? gitops.data.draft_id : undefined} />}
      {editable && <CoolifyImport project={project} configuration={configuration} online={online} />}
      <SectionGrid>
        <GitOpsSource project={project} resources={resources} configuration={configuration} editable={editable} online={online} individualAccount={Boolean(session?.email)} />
        {editable && <ConfigurationExport project={project} environments={environments} online={online} />}
      </SectionGrid>
      <EnvironmentCompare projectId={project.id} environments={environments} />
      {editable && <ProjectStatusShare key={project.id} projectId={project.id} />}
    </>
  );
}
