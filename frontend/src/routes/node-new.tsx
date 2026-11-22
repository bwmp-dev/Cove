import { Server } from "lucide-react";
import { Link, useNavigate } from "@tanstack/react-router";
import { errorMessage } from "../api/client";
import { useCreateNode } from "../api/mutations";
import { EnrollmentPanel } from "../components/EnrollmentPanel";
import { NodeForm } from "../components/NodeForm";
import { Breadcrumbs, Button, EmptyState, Notice, PageHeader } from "../components/ui";
import { SharedNodeWarning } from "../components/Access";
import { useSession } from "../lib/session";
import { buttonClass } from "../lib/styles";

export function NewNodePage() {
  const create = useCreateNode();
  const navigate = useNavigate();
  const admin = useSession()?.is_admin ?? false;
  if (!admin) {
    return (
      <>
        <PageHeader icon={<Server aria-hidden />} title="Add node" />
        <EmptyState title="Instance administrators only">Ask an instance administrator to add a node and assign it to this team.</EmptyState>
      </>
    );
  }

  return (
    <>
      <PageHeader
        icon={<Server aria-hidden />}
        breadcrumbs={<Breadcrumbs><Link to="/nodes" className="rounded hover:text-fg">Nodes</Link>Add node</Breadcrumbs>}
        title="Add node"
        description="Requires Linux with systemd and OpenSSH. On apt-based distributions the installer adds Docker and Compose when they are missing; other distributions need them preinstalled."
      />
      {create.data ? (
        <EnrollmentPanel
          enrollment={create.data}
          nodeId={create.data.id}
          initialState="pending"
          onClose={() => {
            const id = create.data.id;
            create.reset();
            void navigate({ to: "/nodes/$nodeId", params: { nodeId: id } });
          }}
        />
      ) : (
        <div className="space-y-5 px-4 py-5 sm:px-6">
          <SharedNodeWarning />
          <NodeForm disabled={create.isPending} onSubmit={(input) => create.mutate(input)}>
            {create.isError && (
              <Notice tone="error" title="Could not create the node">
                {errorMessage(create.error)}
              </Notice>
            )}
            <div className="flex items-center gap-2">
              <Button type="submit" variant="primary" pending={create.isPending}>
                Generate setup command
              </Button>
              <Link to="/nodes" className={buttonClass("ghost")}>
                Cancel
              </Link>
            </div>
          </NodeForm>
        </div>
      )}
    </>
  );
}
