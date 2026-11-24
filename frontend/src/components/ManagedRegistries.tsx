import { useQuery, useQueryClient, type UseQueryResult } from "@tanstack/react-query";
import { RotateCw } from "lucide-react";
import { useState } from "react";
import { errorMessage } from "../api/client";
import { managedRegistriesQuery, useCreateManagedRegistry, type ManagedRegistryList } from "../api/managed-registries";
import { projectsQuery } from "../api/projects";
import { nodesQuery } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { isLive } from "../lib/managed-registries";
import { ManagedRegistryCreate, type Created } from "./ManagedRegistryCreate";
import { ManagedRegistryItem } from "./ManagedRegistryItem";
import { Button, LoadingRows, Notice, QueryError, Section, Subsection } from "./ui";

const description = "A Docker registry that Cove runs for this team on one of its nodes. Builds and deploys reach it through a generated team registry connection; its credentials stay inside Cove.";

export function ManagedRegistries() {
  const client = useQueryClient();
  const query = useQuery(managedRegistriesQuery(client));
  // Owned here, not by the create form: the form unmounts once the new registry appears in the list.
  const create = useCreateManagedRegistry();
  const [created, setCreated] = useState<Created | null>(null);
  return (
    <Section title="Managed registry" description={description} width="form" flush>
      <RegistriesBody query={query} create={create} created={created} onCreated={setCreated} />
    </Section>
  );
}

interface BodyProps {
  query: UseQueryResult<ManagedRegistryList>;
  create: ReturnType<typeof useCreateManagedRegistry>;
  created: Created | null;
  onCreated: (created: Created | null) => void;
}

function RegistriesBody({ query, create, created, onCreated }: BodyProps) {
  if (query.isPending) return <LoadingRows label="Loading managed registries" rows={3} />;
  if (query.isError && !query.data) {
    return (
      <div className="p-4">
        <QueryError what="managed registries" error={query.error} onRetry={() => void query.refetch()} retrying={query.isFetching} />
      </div>
    );
  }
  return <RegistriesContent query={query} data={query.data} create={create} created={created} onCreated={onCreated} />;
}

function RegistriesContent({ query, data, create, created, onCreated }: BodyProps & { data: ManagedRegistryList }) {
  const online = useOnline();
  const nodes = useQuery(nodesQuery);
  const projects = useQuery(projectsQuery);
  const { registries, can_manage: canManage } = data;
  const blocked = !online ? "You are offline." : query.isError ? "The latest registry status could not be read." : undefined;
  // The new registry is not listed until the refetch after the create lands.
  const awaitingList = created !== null && !registries.some((item) => item.id === created.id);
  const requested = awaitingList || registries.some((item) => item.id === created?.id && isLive(item));
  const creatable = canManage && !registries.some(isLive) && !awaitingList;
  return (
    <div className="divide-y divide-line">
      <PageNotices query={query} canManage={canManage} online={online} requested={requested} />
      {registries.map((registry) => (
        <ManagedRegistryItem
          key={registry.id}
          registry={registry}
          canManage={canManage}
          blocked={blocked}
          nodeName={nodes.data?.find((node) => node.id === registry.node_id)?.name}
          projectName={projects.data?.find((project) => project.id === registry.project_id)?.name}
        />
      ))}
      {creatable && <ManagedRegistryCreate create={create} nodes={nodes} projects={projects} blocked={blocked} onCreated={onCreated} />}
      {!canManage && registries.length === 0 && (
        <Subsection>
          <p className="text-[13px] text-fg-muted">This team has no managed registry. Team administrators can create one.</p>
        </Subsection>
      )}
    </div>
  );
}

function PageNotices({ query, canManage, online, requested }: { query: UseQueryResult<ManagedRegistryList>; canManage: boolean; online: boolean; requested: boolean }) {
  return (
    <div className="space-y-2 p-4 empty:hidden" aria-live="polite">
      {query.isError && <StaleNotice query={query} canManage={canManage} />}
      {canManage && !online && <Notice tone="warn" title="You are offline">Registry changes are paused until the connection returns.</Notice>}
      {requested && (
        <Notice tone="success" title="Managed registry requested">
          Its provisioning deploy is queued in the selected project, where approval, deployment windows and quotas still apply.
        </Notice>
      )}
    </div>
  );
}

function StaleNotice({ query, canManage }: { query: UseQueryResult<ManagedRegistryList>; canManage: boolean }) {
  return (
    <Notice
      tone="error"
      title="Could not refresh managed registries"
      action={
        <Button variant="ghost" size="sm" onClick={() => void query.refetch()} pending={query.isFetching}>
          {!query.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      }
    >
      <p className="break-words">{errorMessage(query.error)}</p>
      <p className="mt-1">{canManage ? "Showing the last status that loaded. Changes are paused until the current status loads; unsaved edits are kept." : "Showing the last status that loaded."}</p>
    </Notice>
  );
}
