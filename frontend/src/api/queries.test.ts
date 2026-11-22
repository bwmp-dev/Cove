import { QueryCache, QueryClient } from "@tanstack/react-query";
import { afterEach, expect, it, vi } from "vitest";
import { rememberTeam, selectedTeam } from "../lib/team";
import { ApiError } from "./client";
import { keys, sessionQuery } from "./queries";

afterEach(() => {
  vi.unstubAllGlobals();
  rememberTeam(null);
});

it("reports a revoked session to the cache error handler instead of retaining authenticated data silently", async () => {
  const onError = vi.fn();
  const client = new QueryClient({ queryCache: new QueryCache({ onError }), defaultOptions: { queries: { retry: false } } });
  client.setQueryData(keys.session, { user_id: "previous-user" });
  rememberTeam("revoked-team");
  vi.stubGlobal("fetch", vi.fn().mockResolvedValue(new Response("unauthorized", { status: 401 })));
  await expect(client.fetchQuery({ ...sessionQuery, staleTime: 0 })).rejects.toBeInstanceOf(ApiError);
  expect(onError).toHaveBeenCalledOnce();
  expect(selectedTeam()).toBeNull();
  client.clear();
});
