import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { useCallback, useRef, useState } from "react";
import { ApiError, errorMessage } from "../api/client";
import { previewQuery, useApproveCIInputs, type EnvironmentPreview } from "../api/previews";
import { workloadsQuery } from "../api/queries";
import { useOnline } from "../lib/hooks";
import { can } from "../lib/permissions";
import { previewStates, reviewableCIInputs, type ReviewedCIInputs } from "../lib/previews";
import { buttonClass, listLinkClass } from "../lib/styles";
import { Dialog } from "./ConfirmDialog";
import { Button, LoadingRows, Notice, QueryError } from "./ui";

// CI approval is bound to the inputs on screen: the dialog reads the preview again when it opens and sends the identity
// it shows, so inputs that arrive while an editor reviews are refused by the server instead of approved unseen.
export function CIApprovalButton({ preview, size, disabled }: { preview: EnvironmentPreview; size: "sm" | "md"; disabled: boolean }) {
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const close = () => {
    setOpen(false);
    trigger.current?.focus();
  };
  return (
    <>
      <button ref={trigger} type="button" className={buttonClass("primary", "", size)} disabled={disabled} onClick={() => setOpen(true)}>
        Review and approve
      </button>
      <Dialog open={open} title={`Review CI inputs for ${preview.slug}`} onClose={close}>
        <CIApprovalReview previewId={preview.id} onClose={close} />
      </Dialog>
    </>
  );
}

function CIApprovalReview({ previewId, onClose }: { previewId: string; onClose: () => void }) {
  const online = useOnline();
  // Mounted only while the dialog is open, so "always" makes every opening read the current inputs, cached or not.
  const detail = useQuery({ ...previewQuery(previewId), refetchOnMount: "always" });
  const approve = useApproveCIInputs();
  const [refused, setRefused] = useState<{ identity: string; message: string } | null>(null);
  // Cancel takes focus when the dialog opens, so approving always needs a deliberate move to the confirmation. The
  // content renders before the dialog is shown, where focus() has no effect; showModal() focuses an autofocus element.
  // React's autoFocus prop calls focus() instead of setting the attribute, so the attribute is set directly.
  const focusCancel = useCallback((node: HTMLButtonElement | null) => node?.setAttribute("autofocus", ""), []);

  const preview = detail.isFetchedAfterMount && !detail.isError ? detail.data : undefined;
  const inputs = preview?.state === "awaiting_approval" ? reviewableCIInputs(preview.ci_inputs) : null;
  const allowed = online && inputs !== null && preview !== undefined && can(preview.permissions, "edit") && !detail.isFetching;

  function confirm() {
    if (!inputs) return;
    const identity = inputs.identity;
    setRefused(null);
    approve.mutate(
      { id: previewId, identity },
      {
        onSuccess: onClose,
        onError: (error) => {
          if (error instanceof ApiError && error.status === 409) setRefused({ identity, message: errorMessage(error) });
        },
      },
    );
  }

  return (
    <div className="space-y-4">
      {detail.isError ? (
        <QueryError what="the current CI inputs" error={detail.error} onRetry={() => void detail.refetch()} retrying={detail.isFetching} />
      ) : preview ? (
        <Review preview={preview} inputs={inputs} />
      ) : (
        <LoadingRows label="Loading the current CI inputs" rows={3} />
      )}
      {refused ? <Refusal message={refused.message} replaced={inputs !== null && inputs.identity !== refused.identity} /> : approve.isError && (
        <Notice tone="error" title="Could not approve the CI inputs">
          {errorMessage(approve.error)}
        </Notice>
      )}
      <div className="flex flex-wrap justify-end gap-2">
        <button ref={focusCancel} type="button" className={buttonClass("ghost")} onClick={onClose} disabled={approve.isPending}>
          Cancel
        </button>
        <Button variant="primary" onClick={confirm} disabled={!allowed} pending={approve.isPending}>
          Approve CI inputs
        </Button>
      </div>
    </div>
  );
}

function Refusal({ message, replaced }: { message: string; replaced: boolean }) {
  return (
    <Notice tone="error" title="The server refused this approval">
      <p>{message}</p>
      <p>{replaced ? "Newer CI inputs replaced the ones you reviewed. Review the inputs above before approving them." : "Nothing was started. Review the current inputs above before trying again."}</p>
    </Notice>
  );
}

function Review({ preview, inputs }: { preview: EnvironmentPreview; inputs: ReviewedCIInputs | null }) {
  if (inputs) return <ReviewedInputs preview={preview} inputs={inputs} />;
  if (preview.state !== "awaiting_approval") {
    // A newer or malformed server may send a state this panel does not know, or none.
    const state = Object.hasOwn(previewStates, String(preview.state)) ? previewStates[preview.state].label.toLowerCase() : "in an unknown state";
    return (
      <Notice tone="warn" title="Nothing to approve">
        This preview is {state}, not awaiting approval.
      </Notice>
    );
  }
  return (
    <Notice tone="error" title="These CI inputs cannot be reviewed">
      The server did not return a complete revision, recipe version and digest-pinned image for every service, so they cannot be approved here. Reload the page, and update the panel if this persists.
    </Notice>
  );
}

function ReviewedInputs({ preview, inputs }: { preview: EnvironmentPreview; inputs: ReviewedCIInputs }) {
  // Names only help recognise a resource; its ID is always shown, so a rename cannot disguise what runs.
  const workloads = useQuery(workloadsQuery);
  const names = new Map((workloads.data ?? []).map((workload) => [workload.id, workload.name]));
  const created = preview.environment_id !== "";
  return (
    <div className="space-y-3">
      <p>{created ? `Approving updates the existing preview ${preview.slug} to these images.` : `Approving creates the preview ${preview.slug} from these images.`}</p>
      <dl className="space-y-3">
        <div>
          <dt className="text-xs text-fg-muted">Revision</dt>
          <dd className="break-all font-mono text-xs text-fg">{inputs.revision}</dd>
        </div>
        <div>
          <dt className="text-xs text-fg-muted">Recipe</dt>
          <dd className="text-fg">Version {inputs.recipeVersion}, frozen for this preview</dd>
        </div>
        <div>
          <dt className="text-xs text-fg-muted">Images</dt>
          <dd>
            <ul aria-label="Images to run" className="mt-1 space-y-2">
              {inputs.resources.map((resource) => {
                const name = names.get(resource.id);
                return (
                  <li key={resource.id} className="min-w-0">
                    <Link to="/resources/$resourceId" params={{ resourceId: resource.id }} className={`${listLinkClass} break-all`}>
                      {name ?? resource.id}
                    </Link>
                    {name !== undefined && <p className="break-all font-mono text-xs text-fg-muted">{resource.id}</p>}
                    <ul className="mt-0.5 space-y-0.5">
                      {resource.images.map(({ service, reference }) => (
                        <li key={service} className="break-all font-mono text-xs">
                          <span className="text-fg">{service}</span> {reference}
                        </li>
                      ))}
                    </ul>
                  </li>
                );
              })}
            </ul>
          </dd>
        </div>
      </dl>
      <p>
        Each image runs with the runtime variables, secret files and data that recipe version {inputs.recipeVersion} permits for its resource. The recipe decides whether storage starts as a copy of production or empty.
        {created && " Image updates keep the preview's data, though preparation that was interrupted earlier may resume."}
      </p>
    </div>
  );
}
