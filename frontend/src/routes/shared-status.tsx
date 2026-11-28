import { useQuery } from "@tanstack/react-query";
import { getRouteApi } from "@tanstack/react-router";
import { RotateCw } from "lucide-react";
import { useLayoutEffect } from "react";
import { ApiError, errorMessage } from "../api/client";
import { publicProjectStatusQuery, type PublicDeploymentState } from "../api/status-share";
import { AuthFrame } from "../components/AuthFrame";
import { RelativeTime } from "../components/RelativeTime";
import { StatusLabel } from "../components/status";
import { Button, LoadingRows, Notice } from "../components/ui";
import type { Tone } from "../lib/nodes";

const route = getRouteApi("/shared/$share");

const states: Record<PublicDeploymentState, { tone: Tone; label: string }> = {
  succeeded: { tone: "ok", label: "Deployed" },
  failed: { tone: "bad", label: "Failed" },
  pending: { tone: "info", label: "Deploying" },
  stopped: { tone: "neutral", label: "Stopped" },
  unknown: { tone: "neutral", label: "Unknown" },
};

const countOrder: PublicDeploymentState[] = ["succeeded", "failed", "pending", "stopped", "unknown"];

// The token is in the address, so for as long as this page is open no request may carry it in a Referer header.
// Changing the existing policy element (rather than adding one) lets the app's policy return when the page closes.
function useNoReferrer() {
  useLayoutEffect(() => {
    let meta = document.querySelector<HTMLMetaElement>('meta[name="referrer"]');
    const added = !meta;
    if (!meta) {
      meta = document.createElement("meta");
      meta.name = "referrer";
      document.head.prepend(meta);
    }
    const previous = meta.content;
    meta.content = "no-referrer";
    return () => {
      if (added) meta.remove();
      else meta.content = previous;
    };
  }, []);
}

function failureMessage(error: unknown) {
  if (error instanceof ApiError) {
    if (error.status === 404) return { title: "This status page is not available", detail: "The link may have expired, been turned off or been replaced with a new one. Ask the person who shared it for a current link." };
    if (error.status === 429) return { title: "Too many requests", detail: error.retryAfter ? `Try again in ${error.retryAfter} seconds.` : "Try again shortly." };
    if (error.status === 503) return { title: "Status is temporarily unavailable", detail: "Try again in a few minutes." };
  }
  // Never echo the request address: it contains the share token.
  return { title: "Could not load the status", detail: errorMessage(error) };
}

export function SharedStatusPage() {
  const { share } = route.useParams();
  useNoReferrer();
  return <SharedStatus key={share} share={share} />;
}

function SharedStatus({ share }: { share: string }) {
  const status = useQuery(publicProjectStatusQuery(share));
  const refresh = (
    <Button size="sm" variant="ghost" onClick={() => void status.refetch()} pending={status.isFetching}>
      {!status.isFetching && <RotateCw aria-hidden className="size-3.5" />}
      Refresh
    </Button>
  );

  return (
    <>
      <title>{status.data ? `${status.data.title} · Deployment status` : "Deployment status"}</title>
      <meta name="robots" content="noindex, nofollow" />
      <AuthFrame title={status.data?.title ?? "Deployment status"} description={status.data ? "Latest deployment results, not live availability." : undefined}>
        {status.isPending ? (
          <LoadingRows label="Loading deployment status" rows={3} />
        ) : status.isError ? (
          <Notice tone={isNotFound(status.error) ? "warn" : "error"} title={failureMessage(status.error).title} action={!isNotFound(status.error) && refresh}>
            {failureMessage(status.error).detail}
          </Notice>
        ) : (
          <div className="space-y-4">
            <div className="flex flex-wrap items-center justify-between gap-2">
              <span className="text-[13px]">
                <span className="sr-only">Overall: </span>
                <StatusLabel tone={states[status.data.state].tone} label={states[status.data.state].label} />
              </span>
              {refresh}
            </div>
            <dl className="divide-y divide-line border-y border-line text-[13px]">
              {countOrder.map((state) => (
                <div key={state} className="flex items-center justify-between gap-4 py-2">
                  <dt>
                    <StatusLabel tone={states[state].tone} label={states[state].label} />
                  </dt>
                  <dd className="tabular-nums text-fg">{status.data.counts[state]}</dd>
                </div>
              ))}
              <div className="flex items-center justify-between gap-4 py-2">
                <dt className="text-fg-muted">Resources</dt>
                <dd className="tabular-nums text-fg">{status.data.counts.total}</dd>
              </div>
            </dl>
            <p className="text-xs text-fg-muted">{status.data.description}</p>
            <p className="text-xs text-fg-muted">
              As of <RelativeTime value={status.data.as_of} />. Refresh to check again.
            </p>
          </div>
        )}
      </AuthFrame>
    </>
  );
}

function isNotFound(error: unknown) {
  return error instanceof ApiError && error.status === 404;
}
