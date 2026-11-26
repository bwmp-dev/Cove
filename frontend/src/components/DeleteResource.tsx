import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { useState } from "react";
import { errorMessage, request } from "../api/client";
import { keys } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button, Notice } from "./ui";

const copy = {
  node: {
    label: "node",
    path: "nodes",
    description: "Cove stops managing this node and removes its credentials. The node must have no resources, build node assignments or builds still recorded for a resource. The installed worker is not uninstalled.",
    confirm: "The node must have no resources, build node assignments or builds still recorded for a resource. Cove removes its credentials and stops managing it. The installed worker is not uninstalled.",
  },
  workload: {
    label: "resource",
    path: "workloads",
    description: "Stop the resource first. Volumes, backups and the metadata needed to restore them stay on the node. Other worker files and unused build images with Cove-generated names are removed; worker files are kept if their storage layout cannot be verified.",
    confirm:
      "Stop the resource first. Cove removes its routes, stopped containers, API tokens, dashboard history, other worker files and unused build images with Cove-generated names. Volumes, backups and the metadata needed to restore them stay on the node, as do images you named, including ones Cove built. If Cove cannot verify the worker's storage layout, it keeps all worker files.",
  },
} as const;

export function DeleteResource({ kind, id }: { kind: "node" | "workload"; id: string }) {
  const [confirm, setConfirm] = useState(false);
  const client = useQueryClient();
  const navigate = useNavigate();
  const online = useOnline();
  const text = copy[kind];
  const deletion = useMutation({
    mutationFn: () => request("DELETE", `/api/${text.path}/${encodeURIComponent(id)}`),
    onSuccess: async () => {
      await Promise.all([
        client.invalidateQueries({ queryKey: keys.workloads }),
        client.invalidateQueries({ queryKey: keys.nodes }),
        client.invalidateQueries({ queryKey: keys.operations }),
      ]);
      await navigate({ to: kind === "node" ? "/nodes" : "/resources" });
    },
  });
  return (
    <div className="space-y-3">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <p className="max-w-xl text-[13px] text-fg-muted">{text.description}</p>
        <Button variant="danger" disabled={!online} pending={deletion.isPending} onClick={() => setConfirm(true)}>
          Delete {text.label}
        </Button>
      </div>
      {deletion.isError && <Notice tone="error">{errorMessage(deletion.error)}</Notice>}
      <ConfirmDialog
        open={confirm}
        title={`Delete this ${text.label}?`}
        confirmLabel={`Delete ${text.label}`}
        danger
        onCancel={() => setConfirm(false)}
        onConfirm={() => {
          setConfirm(false);
          deletion.mutate();
        }}
      >
        {text.confirm}
      </ConfirmDialog>
    </div>
  );
}
