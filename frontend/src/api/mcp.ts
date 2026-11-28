import { queryOptions, useMutation, useQueryClient } from "@tanstack/react-query";
import { request } from "./client";

export interface McpSettings { enabled: boolean; url: string }

export const mcpSettingsQuery = queryOptions({
  queryKey: ["mcp-settings"],
  queryFn: ({ signal }) => request<McpSettings>("GET", "/api/mcp", undefined, signal),
});

export function useUpdateMcpSettings() {
  const client = useQueryClient();
  return useMutation({
    mutationFn: (enabled: boolean) => request<McpSettings>("PUT", "/api/mcp", { enabled }),
    onSuccess: (settings) => client.setQueryData(mcpSettingsQuery.queryKey, settings),
  });
}
