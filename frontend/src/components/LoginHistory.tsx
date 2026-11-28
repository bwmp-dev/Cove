import { useQuery } from "@tanstack/react-query";
import { loginHistoryQuery } from "../api/security";
import { RelativeTime } from "./RelativeTime";
import { StatusLabel } from "./status";
import { LoadingRows, QueryError } from "./ui";
import { tableHeadClass, tableRowClass } from "../lib/styles";

const methodLabels: Record<string, string> = {
  login: "Password",
  "second-factor": "Authenticator or recovery code",
  "passkeys/finish": "Passkey",
  github: "GitHub",
  oidc: "Single sign-on",
  "forgot-password": "Password reset request",
  "reset-password": "Password reset",
};

export function loginMethodLabel(method: string) {
  return methodLabels[method] ?? method;
}

// userId null reads the signed-in account's own history.
export function LoginHistory({ userId }: { userId: string | null }) {
  const history = useQuery(loginHistoryQuery(userId));
  if (history.isPending) return <LoadingRows label="Loading sign-in history" rows={3} />;
  if (history.isError) {
    return (
      <div className="p-3">
        <QueryError what="sign-in history" error={history.error} onRetry={() => void history.refetch()} retrying={history.isFetching} />
      </div>
    );
  }
  if (history.data.length === 0) return <p className="px-4 py-3 text-[13px] text-fg-muted">No sign-in attempts in the last 90 days.</p>;
  return (
    <table className="w-full table-fixed text-left text-[13px]">
      <caption className="sr-only">Sign-in attempts, most recent first</caption>
      <thead>
        <tr className={tableHeadClass}>
          <th scope="col" className="w-28 pl-4">
            Result
          </th>
          <th scope="col" className="pr-4 pl-2">
            Method
          </th>
          <th scope="col" className="hidden w-36 pr-4 md:table-cell">
            Address
          </th>
          <th scope="col" className="hidden pr-4 lg:table-cell">
            Browser
          </th>
          <th scope="col" className="w-28 pr-4 text-right">
            When
          </th>
        </tr>
      </thead>
      <tbody>
        {history.data.map((event) => (
          <tr key={event.id} className={tableRowClass}>
            <td className="py-2 pl-4 align-top">
              <StatusLabel tone={event.success ? "ok" : "bad"} label={event.success ? "Accepted" : "Rejected"} />
            </td>
            <td className="min-w-0 py-2 pr-4 pl-2 align-top text-fg">
              <p className="break-words">{loginMethodLabel(event.method)}</p>
              <p className="font-mono text-xs break-all text-fg-muted md:hidden">{event.address}</p>
            </td>
            <td className="hidden py-2 pr-4 align-top font-mono text-xs break-all text-fg-muted md:table-cell">{event.address}</td>
            <td className="hidden py-2 pr-4 align-top text-xs text-fg-muted lg:table-cell">
              <p className="truncate" title={event.user_agent}>
                {event.user_agent || "Not reported"}
              </p>
            </td>
            <td className="py-2 pr-4 text-right align-top text-xs text-fg-muted tabular-nums">
              <RelativeTime value={event.created_at} />
            </td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

export const loginHistoryDescription =
  "The 100 most recent attempts from the last 90 days. An accepted password can still be followed by a second-factor step, so read each method separately.";
