import { useQuery, useQueryClient, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { RotateCw } from "lucide-react";
import { useState, type FormEvent, type ReactNode } from "react";
import { buildCacheKey, buildCacheQuery, useConfigureBuildCache, type BuildCacheStatus } from "../api/build-cache";
import { ApiError, isStatus } from "../api/client";
import type { Node } from "../api/types";
import { cachePathError, formatBytes, maxCachePathLength } from "../lib/build-cache";
import { useOnline } from "../lib/hooks";
import { useSession } from "../lib/session";
import { inputClass, linkClass } from "../lib/styles";
import { ConfirmDialog } from "./ConfirmDialog";
import { Button, DefinitionList, Field, LoadingRows, Meter, MeterGroup, Notice, Section } from "./ui";

const description = "A hard cap on Cove's BuildKit cache, enforced by a dedicated filesystem you provide. It applies to builds for every team on this node.";

const noCap = "Builds use Docker's ordinary garbage collection and Cove's scheduled pruning. They reclaim space but do not limit it, so this is not a hard cap.";

const outsideCap = "The legacy Cove builder cache, Docker daemon images, releases and registry storage are outside this cap.";

type Configure = UseMutationResult<BuildCacheStatus, Error, string>;

function readError(error: unknown) {
  if (error instanceof ApiError || error instanceof Error) return error.message;
  return "Something went wrong.";
}

const enforced = (status: BuildCacheStatus) => status.configured && status.verified && status.capacity_bytes > 0 && status.available_bytes <= status.capacity_bytes;

function savedMessage(path: string, result: BuildCacheStatus, wasConfigured: boolean) {
  if (path === "" && !wasConfigured) return result.cleanup_pending ? "Cleanup is still unfinished. Check the status below before retrying." : "Cleanup finished. No hard cap is configured.";
  if (path === "") return "Hard cap disabled. The cache data was kept, and builds use ordinary garbage collection with no hard cap.";
  if (enforced(result)) return `Hard cap verified. Builds on this node now use ${path}.`;
  return `${path} was saved but is not verified. Builds stay paused until it verifies.`;
}

// Pending cleanup is not a blocker: it is left over from an earlier change and the worker retries it before applying
// a setting, so verifying or disabling again is how an administrator retries it.
function sharedBlocker(status: BuildCacheStatus, online: boolean) {
  if (!online) return "You are offline.";
  if (status.active_operation_id) return "An operation is running on this node.";
  return undefined;
}

// Disabling stays possible when the worker cannot configure a new filesystem, so a broken saved cap can be removed.
function verifyBlocker(status: BuildCacheStatus, online: boolean, draft: string) {
  const shared = sharedBlocker(status, online);
  if (shared) return shared;
  if (!status.can_configure) return "This worker cannot verify a dedicated filesystem.";
  if (draft === status.path && enforced(status) && !status.cleanup_pending) return "This filesystem is already verified and in use.";
  return undefined;
}

export function BuildCacheSection({ node }: { node: Node }) {
  const admin = useSession()?.is_admin ?? false;
  const status = useQuery({ ...buildCacheQuery(node.id), enabled: admin && node.capabilities?.build_cache_limits === true && node.state === "ready" });
  const client = useQueryClient();
  const configure = useConfigureBuildCache(node.id);
  const [draft, setDraft] = useState<string | null>(null);
  const [checked, setChecked] = useState(false);
  const [confirming, setConfirming] = useState<"use" | "disable" | null>(null);
  const [saved, setSaved] = useState<string | null>(null);
  if (!admin) return null;
  const savedPath = status.data?.path ?? "";
  const value = draft ?? savedPath;

  function edit(next: string) {
    setDraft(next);
    setChecked(false);
    setSaved(null);
    configure.reset();
  }

  function submit(event: FormEvent<HTMLFormElement>) {
    event.preventDefault();
    setChecked(true);
    if (!cachePathError(value)) setConfirming("use");
  }

  function apply(path: string) {
    const wasConfigured = status.data?.configured ?? true;
    configure.mutate(path, {
      onSuccess: (result) => {
        client.setQueryData(buildCacheKey(node.id), result);
        setDraft(null);
        setChecked(false);
        setSaved(savedMessage(path, result, wasConfigured));
      },
      onSettled: () => setConfirming(null),
    });
  }

  return (
    <Section title="Build cache" description={description}>
      <div className="space-y-4">
        <ConfigureNotices configure={configure} saved={saved} />
        <SectionState node={node} status={status}>
          {(data) => <BuildCacheBody node={node} status={data} draft={value} checked={checked} pending={configure.isPending} onEdit={edit} onUse={submit} onDisable={() => setConfirming("disable")} />}
        </SectionState>
      </div>
      <ConfirmDialog open={confirming === "use"} title="Use this filesystem for the build cache?" confirmLabel="Verify and use filesystem" pending={configure.isPending} onConfirm={() => apply(value)} onCancel={() => setConfirming(null)}>
        <p>
          Cove verifies <span className="font-mono break-all text-fg">{value}</span> and makes its size the hard cap for every team's builds on {node.name}.
        </p>
        <p className="mt-2">
          New builds pause while it switches. Any previous capped builder is stopped and its cache data is kept, not deleted. The existing legacy Cove builder cache is left as it is, outside the cap. If the filesystem changes after it is saved, builds are refused rather than run without the cap.
        </p>
      </ConfirmDialog>
      {status.data?.configured === false ? (
        <ConfirmDialog open={confirming === "disable"} title="Retry build-cache cleanup?" confirmLabel="Retry cleanup" pending={configure.isPending} onConfirm={() => apply("")} onCancel={() => setConfirming(null)}>
          <p>Cove retries stopping the previous capped builder on {node.name}. No hard cap is configured, and none is added.</p>
          <p className="mt-2">The builder's cache data is kept. Nothing is deleted.</p>
        </ConfirmDialog>
      ) : (
        <ConfirmDialog open={confirming === "disable"} title="Disable the hard build-cache cap?" confirmLabel="Disable hard cap" danger pending={configure.isPending} onConfirm={() => apply("")} onCancel={() => setConfirming(null)}>
          <p>Builds on {node.name} go back to Docker's ordinary garbage collection, which has no hard cap.</p>
          <p className="mt-2">
            Cove stops its capped builder and keeps the data on <span className="font-mono break-all text-fg">{savedPath}</span>. Nothing is deleted.
          </p>
        </ConfirmDialog>
      )}
    </Section>
  );
}

function ConfigureNotices({ configure, saved }: { configure: Configure; saved: string | null }) {
  const disabling = configure.variables === "";
  return (
    <div className="space-y-2 empty:hidden" aria-live="polite">
      {saved && <Notice tone="success">{saved}</Notice>}
      {configure.isError && (
        <Notice tone={isStatus(configure.error, 409) || isStatus(configure.error, 400) ? "warn" : "error"} title={disabling ? "Disabling did not complete" : "Verifying the filesystem did not complete"}>
          <p className="break-words">{readError(configure.error)}</p>
          <p className="mt-1">
            {disabling ? "" : "Your path is kept. "}
            Check the refreshed status: the worker may have saved the setting or its ownership marker before the reply failed.
          </p>
        </Notice>
      )}
    </div>
  );
}

function SectionState({ node, status, children }: { node: Node; status: UseQueryResult<BuildCacheStatus>; children: (data: BuildCacheStatus) => ReactNode }) {
  if (node.capabilities?.build_cache_limits !== true) {
    return (
      <Notice tone="warn" title="Worker upgrade required">
        <p>Upgrade and verify this node's worker to configure a hard build-cache cap.</p>
        <p className="mt-1">Whether a hard cap is enforced on this node is unknown until the worker is upgraded and verified.</p>
      </Notice>
    );
  }
  if (node.state !== "ready") {
    return <Notice tone="warn" title="Build-cache state unknown">The node is not ready, so Cove cannot read or change its build cache. Verify the node first.</Notice>;
  }
  if (status.isPending) return <LoadingRows label="Loading build cache" rows={2} />;
  if (status.isError) return <ReadError status={status} />;
  return children(status.data);
}

// A failed read never falls back to the last reading, so a cap that stopped verifying cannot still look enforced.
function ReadError({ status }: { status: UseQueryResult<BuildCacheStatus> }) {
  const unavailable = isStatus(status.error, 502);
  return (
    <Notice
      tone={unavailable ? "warn" : "error"}
      title={unavailable ? "Worker unavailable" : "Build-cache state unknown"}
      action={
        <Button variant="ghost" size="sm" onClick={() => void status.refetch()} pending={status.isFetching}>
          {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
          Retry
        </Button>
      }
    >
      <p className="break-words">{readError(status.error)}</p>
      <p className="mt-1">Whether a hard cap is enforced is unknown until the status loads. Changes are paused.</p>
    </Notice>
  );
}

interface BodyProps {
  node: Node;
  status: BuildCacheStatus;
  draft: string;
  checked: boolean;
  pending: boolean;
  onEdit: (value: string) => void;
  onUse: (event: FormEvent<HTMLFormElement>) => void;
  onDisable: () => void;
}

function BuildCacheBody({ status, ...form }: BodyProps) {
  const online = useOnline();
  return (
    <div className="space-y-4">
      <CacheState status={status} />
      {status.active_operation_id && (
        <Notice
          tone="warn"
          title="An operation is running on this node"
          action={
            <Link to="/operations/$operationId" params={{ operationId: status.active_operation_id }} className={linkClass}>
              View operation
            </Link>
          }
        >
          Build-cache settings cannot change until queued and running operations on this node finish, including builds for resources on other nodes.
        </Notice>
      )}
      {status.cleanup_pending && (
        <Notice tone="warn" title="Unfinished build-cache cleanup">
          {status.configured
            ? "Cleanup of a previous Cove-owned capped builder did not finish, and new builds stay paused. Verify the saved path again or disable the cap to retry it."
            : "Cleanup of a previous Cove-owned capped builder did not finish, and new builds stay paused. Retry cleanup to try again."}
        </Notice>
      )}
      {!online && <Notice tone="warn" title="You are offline">Changes are paused until the connection returns.</Notice>}
      <DefinitionList
        items={[
          { term: "Saved path", value: status.path ? <span className="font-mono text-xs break-all">{status.path}</span> : <span className="text-fg-subtle">None</span> },
          { term: "BuildKit image", value: status.image ? <span className="font-mono text-xs break-all">{status.image}</span> : <span className="text-fg-subtle">Not reported</span> },
          { term: "Scope", value: "Every team on this node" },
        ]}
      />
      <p className="text-xs text-fg-muted">{outsideCap}</p>
      {status.can_manage && <CacheForm status={status} online={online} {...form} />}
    </div>
  );
}

function CacheForm({ node, status, online, draft, checked, pending, onEdit, onUse, onDisable }: BodyProps & { online: boolean }) {
  const pathError = checked ? cachePathError(draft) : undefined;
  const disableBlocked = Boolean(sharedBlocker(status, online)) || pending;
  const blocked = verifyBlocker(status, online, draft);
  const id = `build-cache-path-${node.id}`;
  return (
    <form onSubmit={onUse} noValidate aria-label="Build-cache filesystem" className="space-y-3 border-t border-line pt-4">
      <Field
        id={id}
        label="Filesystem mount path"
        error={pathError}
        hint="An existing, writable mount with mode 0700 that holds only Cove's BuildKit state, on a different filesystem from / and Docker's data root. The worker needs a local Unix Docker endpoint and the host's filesystem namespace. Cove verifies this; it never creates, mounts, resizes or deletes storage. The filesystem's size is the cap. To use a smaller filesystem, disable the cap first."
      >
        <input
          id={id}
          name="path"
          type="text"
          className={inputClass}
          value={draft}
          maxLength={maxCachePathLength}
          placeholder="/mnt/cove-buildkit"
          autoComplete="off"
          spellCheck={false}
          disabled={pending}
          aria-invalid={pathError ? true : undefined}
          aria-describedby={pathError ? `${id}-error` : `${id}-hint`}
          onChange={(event) => onEdit(event.target.value)}
        />
      </Field>
      <div className="flex flex-wrap items-center justify-end gap-2">
        {status.configured && (
          <Button variant="danger" disabled={disableBlocked} onClick={onDisable}>
            Disable hard cap
          </Button>
        )}
        {!status.configured && status.cleanup_pending && (
          <Button disabled={disableBlocked} onClick={onDisable}>
            Retry cleanup
          </Button>
        )}
        <Button type="submit" variant="primary" disabled={Boolean(blocked) || pending} aria-describedby={blocked ? `${id}-blocked` : undefined}>
          Verify and use filesystem
        </Button>
      </div>
      {blocked && (
        <p id={`${id}-blocked`} className="text-right text-xs text-fg-muted">
          {blocked}
        </p>
      )}
    </form>
  );
}

function CacheState({ status }: { status: BuildCacheStatus }) {
  if (enforced(status)) {
    return (
      <div className="space-y-3">
        <Notice tone="success" title="Hard cap enforced">
          Builds can use at most this filesystem. When it is full, builds fail with a clear error; Cove prunes only its own cache and never daemon images, releases or registry storage.
        </Notice>
        <MeterGroup label="Build-cache filesystem" columns={3}>
          <Meter label="Cache filesystem used" value={status.capacity_bytes - status.available_bytes} limit={status.capacity_bytes} format={formatBytes} detail={`${formatBytes(status.available_bytes)} available`} />
        </MeterGroup>
      </div>
    );
  }
  if (status.configured) {
    return (
      <Notice tone="warn" title="Saved filesystem not verified">
        <p className="break-words">{status.reason || "The worker did not confirm the saved filesystem."}</p>
        <p className="mt-1">Builds on this node are refused until it verifies; Cove does not fall back to an uncapped cache. Fix the filesystem and verify it again, or disable the cap.</p>
      </Notice>
    );
  }
  return (
    <Notice title="No hard cap">
      <p>{noCap}</p>
      {status.reason && <p className="mt-1 break-words">{status.reason}</p>}
    </Notice>
  );
}
