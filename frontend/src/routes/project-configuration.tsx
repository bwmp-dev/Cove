import { useQuery } from "@tanstack/react-query";
import type { Project } from "../api/projects";
import { gitOpsQuery, projectConfigurationQuery } from "../api/project-configuration";
import { ConfigurationDrafts } from "../components/ConfigurationDrafts";
import { ConfigurationExport } from "../components/ConfigurationExport";
import { CoolifyImport } from "../components/CoolifyImport";
import { EnvironmentCompare } from "../components/EnvironmentCompare";
import { GitOpsSource } from "../components/GitOpsSource";
import { ProjectManifest } from "../components/ProjectManifest";
import { ProjectStatusShare } from "../components/ProjectStatusShare";
import { EmptyState, Notice, SectionGrid } from "../components/ui";
import { canCompareEnvironments, canExportConfiguration, productionEnvironments } from "../lib/hierarchy";
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
  // Export is a secret read, independent of edit access: a custom role can hold either one without the other.
  const exportable = canExportConfiguration(project);
  const configuration = useQuery({ ...projectConfigurationQuery(project.id), enabled: readable });
  const gitops = useQuery({ ...gitOpsQuery(project.id), enabled: readable });
  const environments = productionEnvironments(project);

  if (!readable && (canCompareEnvironments(project) || exportable)) {
    return <EnvironmentScopedConfiguration project={project} online={online} />;
  }
  if (!readable) {
    return <EmptyState title="Configuration needs read access">Ask a team owner or admin for access to this project.</EmptyState>;
  }
  return (
    <>
      {(!online || !editable || !exportable) && (
        <div className="space-y-2 px-4 pt-5 sm:px-6">
          {!online && <Notice tone="warn" title="You are offline">Changes are unavailable until the connection returns.</Notice>}
          {!editable && (
            <Notice>
              You can view this configuration. Saving drafts, importing and changing GitOps settings need editor access to the project
              {exportable ? "." : "; exporting with secrets needs permission to read secrets."}
            </Notice>
          )}
          {editable && !exportable && <Notice>Exporting with secrets needs permission to read secrets in the project or one of its environments.</Notice>}
        </div>
      )}
      <ProjectManifest configuration={configuration} />
      {editable && <ConfigurationDrafts projectId={project.id} currentVersion={configuration.data?.version} online={online} gitOpsDraft={gitops.data?.connected ? gitops.data.draft_id : undefined} />}
      {editable && <CoolifyImport project={project} configuration={configuration} online={online} />}
      <SectionGrid>
        <GitOpsSource project={project} resources={resources} configuration={configuration} editable={editable} online={online} individualAccount={Boolean(session?.email)} />
        {exportable && <ConfigurationExport project={project} online={online} />}
      </SectionGrid>
      <EnvironmentCompare projectId={project.id} environments={environments} />
      {editable && <ProjectStatusShare key={project.id} projectId={project.id} />}
    </>
  );
}

// Environment grants without project read: comparing needs read on two environments, exporting needs read_secrets on one.
function EnvironmentScopedConfiguration({ project, online }: { project: Project; online: boolean }) {
  const comparable = canCompareEnvironments(project);
  const exportable = canExportConfiguration(project);
  const allowed = comparable && exportable
    ? "You can compare the environments you can read and export those whose secrets you can read."
    : comparable
      ? "You can compare the environments you can read."
      : "You can export the environments whose secrets you can read.";
  return (
    <>
      <div className="px-4 pt-5 sm:px-6">
        <Notice>{allowed} The project's configuration, drafts and GitOps need read access to the project.</Notice>
      </div>
      {exportable && (
        <SectionGrid>
          <ConfigurationExport project={project} online={online} />
        </SectionGrid>
      )}
      {comparable && <EnvironmentCompare projectId={project.id} environments={productionEnvironments(project)} />}
    </>
  );
}
