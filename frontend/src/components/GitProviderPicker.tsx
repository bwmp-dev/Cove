import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useId } from "react";
import { gitConnectionsQuery, providerBranchesQuery, providerLabels, providerRepositoriesQuery, type ProviderRepository, type ProviderSelection } from "../api/git-providers";
import { inputClass } from "../lib/styles";
import { Select } from "./Select";
import { Field, LoadingRows, QueryError } from "./ui";

interface PickerProps {
  value: ProviderSelection;
  onChange: (value: ProviderSelection) => void;
  onRepository?: (repository: ProviderRepository | undefined) => void;
  disabled?: boolean;
}

function BranchField({ id, connectionId, repositoryId, value, onChange, disabled }: PickerProps & { id: string; connectionId: number; repositoryId: number }) {
  const branches = useQuery(providerBranchesQuery(connectionId, repositoryId));
  if (branches.isError) return <QueryError what="branches" error={branches.error} onRetry={() => void branches.refetch()} retrying={branches.isFetching} />;
  const hint = branches.isPending ? "Loading branches" : branches.data.length === 0 ? "The repository has no branches." : undefined;
  // Keep a saved branch selectable even if the provider no longer lists it, so the form shows what is saved.
  const options = branches.data && value.branch && !branches.data.includes(value.branch) ? [value.branch, ...branches.data] : branches.data;
  return (
    <Field id={`${id}-branch`} label="Branch" hint={hint}>
      <Select id={`${id}-branch`} className={`${inputClass} font-mono`} value={value.branch} disabled={disabled || branches.isPending} aria-describedby={hint ? `${id}-branch-hint` : undefined} onChange={(event) => onChange({ ...value, branch: event.target.value })}>
        <option value="">Select a branch</option>
        {options?.map((branch) => <option key={branch} value={branch}>{branch}</option>)}
      </Select>
    </Field>
  );
}

function RepositoryFields({ id, connectionId, ...props }: PickerProps & { id: string; connectionId: number }) {
  const { value, onChange, onRepository, disabled } = props;
  const repositories = useQuery(providerRepositoriesQuery(connectionId));
  if (repositories.isPending) return <LoadingRows label="Loading repositories" rows={1} />;
  if (repositories.isError) return <QueryError what="repositories" error={repositories.error} onRetry={() => void repositories.refetch()} retrying={repositories.isFetching} />;
  if (repositories.data.length === 0) return <p className="text-[13px] text-fg-muted">This connection's token cannot see any repositories.</p>;
  return (
    <div className="grid gap-4 sm:grid-cols-2">
      <Field id={`${id}-repository`} label="Repository">
        <Select
          id={`${id}-repository`}
          value={value.repositoryId ?? ""}
          disabled={disabled}
          onChange={(event) => {
            const repository = repositories.data.find((item) => String(item.id) === event.target.value);
            onChange({ ...value, repositoryId: repository?.id ?? null, branch: repository?.default_branch ?? "" });
            onRepository?.(repository);
          }}
        >
          <option value="">Select a repository</option>
          {repositories.data.map((repository) => <option key={repository.id} value={repository.id}>{repository.full_name}</option>)}
        </Select>
      </Field>
      {value.repositoryId !== null && <BranchField id={id} connectionId={connectionId} repositoryId={value.repositoryId} {...props} />}
    </div>
  );
}

// Connection, repository and branch choices backed by the provider's own lists.
export function GitProviderPicker(props: PickerProps) {
  const { value, onChange, onRepository, disabled } = props;
  const id = useId();
  const connections = useQuery(gitConnectionsQuery);
  if (connections.isPending) return <LoadingRows label="Loading Git providers" rows={1} />;
  if (connections.isError) return <QueryError what="Git providers" error={connections.error} onRetry={() => void connections.refetch()} retrying={connections.isFetching} />;
  if (connections.data.length === 0) {
    return (
      <p className="text-[13px] text-fg-muted">
        No GitLab, Gitea, Forgejo, Codeberg or Bitbucket Cloud connections. Connect one under{" "}
        <Link to="/integrations" className="text-fg underline underline-offset-4">Integrations</Link>.
      </p>
    );
  }
  return (
    <div className="space-y-4">
      <Field id={`${id}-connection`} label="Connection">
        <Select
          id={`${id}-connection`}
          value={value.connectionId ?? ""}
          disabled={disabled}
          onChange={(event) => {
            onChange({ connectionId: event.target.value ? Number(event.target.value) : null, repositoryId: null, branch: "" });
            onRepository?.(undefined);
          }}
        >
          <option value="">Select a connection</option>
          {connections.data.map((connection) => <option key={connection.id} value={connection.id}>{`${connection.name} (${providerLabels[connection.kind]})`}</option>)}
        </Select>
      </Field>
      {value.connectionId !== null && <RepositoryFields id={id} connectionId={value.connectionId} {...props} />}
    </div>
  );
}
