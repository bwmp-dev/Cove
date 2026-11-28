import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { activityQuery, type ActivityItem } from "../api/organization";
import type { OperationAction, OperationState } from "../api/types";
import { activityLink } from "../lib/organization";
import { actionLabels, stateLabels } from "../lib/operations";
import { listLinkClass } from "../lib/styles";
import { RelativeTime } from "./RelativeTime";
import { OperationStateLabel, StatusLabel } from "./status";
import { EmptyState, LoadingRows, QueryError, Section } from "./ui";

function isOperationState(state: string): state is OperationState {
  return state in stateLabels;
}

function actionLabel(item: ActivityItem) {
  if (item.kind === "operation") return item.action in actionLabels ? actionLabels[item.action as OperationAction] : item.action;
  return "Configuration change";
}

function ActivityState({ item }: { item: ActivityItem }) {
  if (isOperationState(item.state)) return <OperationStateLabel state={item.state} />;
  return <StatusLabel tone="neutral" label={item.state} />;
}

function ActivityRow({ item }: { item: ActivityItem }) {
  const link = activityLink(item.path);
  const name = <span className="break-words">{item.name}</span>;
  return (
    <li className="flex flex-wrap items-center gap-x-4 gap-y-1 px-3 py-2.5 hover:bg-raised">
      <div className="min-w-0 flex-1 basis-48">
        <p className="text-[13px] text-fg">
          {link ? (
            <Link {...link} className={listLinkClass}>
              {name}
            </Link>
          ) : (
            name
          )}
        </p>
        <p className="mt-0.5 text-xs text-fg-muted">
          {actionLabel(item)}
          {item.kind === "configuration" && <span className="ml-1.5 font-mono break-all text-fg-subtle">{item.action}</span>}
        </p>
      </div>
      <ActivityState item={item} />
      <RelativeTime value={item.created_at} className="w-24 text-right text-xs text-fg-muted tabular-nums" />
    </li>
  );
}

// Names and links are filtered by the server to the viewer's current grants; actors and values are never included.
export function ActivityFeed() {
  const activity = useQuery(activityQuery);
  let body;
  if (activity.isPending) {
    body = <LoadingRows label="Loading activity" rows={3} />;
  } else if (activity.isError && !activity.data) {
    body = (
      <div className="p-3">
        <QueryError what="activity" error={activity.error} onRetry={() => void activity.refetch()} retrying={activity.isFetching} />
      </div>
    );
  } else if (activity.data.length === 0) {
    body = <EmptyState title="No activity yet">Deployments, lifecycle operations and configuration changes on resources you can access appear here.</EmptyState>;
  } else {
    body = (
      <>
        {activity.isRefetchError && (
          <div className="border-b border-line p-3">
            <QueryError what="the latest activity" error={activity.error} onRetry={() => void activity.refetch()} retrying={activity.isFetching} />
          </div>
        )}
        <ul aria-label="Activity" className="divide-y divide-line">
          {activity.data.map((item) => (
            <ActivityRow key={`${item.kind}-${item.id}`} item={item} />
          ))}
        </ul>
      </>
    );
  }
  return (
    <Section title="Activity" description="The 50 most recent operations and configuration changes you can see." flush>
      {body}
    </Section>
  );
}
