import { HardDrive } from "lucide-react";
import { errorMessage, isStatus } from "../api/client";
import type { useEnableLocalNode } from "../api/mutations";
import { Button, Notice } from "./ui";

type LocalNodeMutation = ReturnType<typeof useEnableLocalNode>;

export function LocalNodeButton({ mutation }: { mutation: LocalNodeMutation }) {
  return (
    <Button variant="secondary" pending={mutation.isPending} onClick={() => mutation.mutate()}>
      {!mutation.isPending && <HardDrive aria-hidden className="size-3.5" />}
      Use main node
    </Button>
  );
}

export function LocalNodeResult({ mutation }: { mutation: LocalNodeMutation }) {
  if (mutation.isError) {
    return isStatus(mutation.error, 409) ? (
      <Notice tone="warn" title="The local worker on the main node is not available">
        The host operator must install and start the local worker and set COVE_LOCAL_SOCKET on the control plane before the main
        node can run workloads.
      </Notice>
    ) : (
      <Notice tone="error" title="Could not enable the main node">
        {errorMessage(mutation.error)}
      </Notice>
    );
  }
  if (mutation.isSuccess) {
    return <Notice tone="success" title="The main node can now run workloads" />;
  }
  return null;
}
