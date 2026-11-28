import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { errorMessage } from "../api/client";
import { mcpSettingsQuery, useUpdateMcpSettings } from "../api/mcp";
import { useOnline } from "../lib/hooks";
import { linkClass } from "../lib/styles";
import { CopyButton } from "./CopyButton";
import { StatusLabel } from "./status";
import { Button, LoadingRows, Notice, QueryError, SettingRow, SettingRows } from "./ui";

export function McpSettings() {
  const settings = useQuery(mcpSettingsQuery);
  const update = useUpdateMcpSettings();
  const online = useOnline();
  if (settings.isPending) return <LoadingRows label="Loading MCP settings" rows={2} />;
  if (settings.isError) return <QueryError what="MCP settings" error={settings.error} onRetry={() => void settings.refetch()} retrying={settings.isFetching} />;
  const { enabled, url } = settings.data;
  return (
    <div className="space-y-4">
      <SettingRows>
        <SettingRow
          label="MCP endpoint"
          description={
            enabled
              ? "AI assistants and other MCP clients can connect with a scoped API key."
              : "Off. The endpoint answers 404 until it is turned on."
          }
        >
          <div className="flex flex-wrap items-center gap-3">
            <StatusLabel tone={enabled ? "ok" : "neutral"} label={enabled ? "On" : "Off"} />
            <Button disabled={!online} pending={update.isPending} onClick={() => update.mutate(!enabled)}>
              {enabled ? "Turn off" : "Turn on"}
            </Button>
          </div>
        </SettingRow>
        <SettingRow label="Server URL" description="Streamable HTTP. Configure this URL in the MCP client.">
          <div className="flex min-w-0 flex-wrap items-center gap-2">
            <span className="min-w-0 font-mono text-xs break-all text-fg">{url}</span>
            <CopyButton text={url} label="Copy URL" />
          </div>
        </SettingRow>
      </SettingRows>
      <div className="space-y-1.5 text-xs text-fg-muted">
        <p>
          Clients authenticate with a personal or team API key sent as <span className="font-mono whitespace-nowrap">Authorization: Bearer</span>. Session cookies, resource tokens and the administrator token are refused, and browser requests from other origins are rejected.
        </p>
        <p>
          The key's permissions decide what is offered: projects and resources to read, and deploy, restart and log tools. Every call also rechecks the key creator's current access, so removing a person's access takes effect immediately.{" "}
          <Link to="/tokens" className={linkClass}>
            Manage API keys
          </Link>
        </p>
      </div>
      {update.isError && (
        <Notice tone="error" title="Could not change the MCP setting">
          {errorMessage(update.error)}
        </Notice>
      )}
    </div>
  );
}
