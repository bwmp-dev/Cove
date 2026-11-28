import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useSyncExternalStore, type ReactNode } from "react";
import { keys, type Session } from "../api/queries";
import { timezonePreferenceQuery } from "../api/timezone";
import { setDisplayTimeZone } from "../lib/timezone";

// Reads the session from the cache rather than through a query observer: signing out or switching teams clears the
// cache, which would leave an observer holding the previous team's zone. Leaving the signed-in app (for a public page)
// drops the zone so public pages show the browser's zone.
export function TimezoneProvider({ children }: { children: ReactNode }) {
  const client = useQueryClient();
  const subscribe = useCallback((callback: () => void) => client.getQueryCache().subscribe(callback), [client]);
  const session = useSyncExternalStore(subscribe, () => client.getQueryData<Session | null>(keys.session));
  const teamId = session?.team_id ?? "";
  const preference = useQuery({ ...timezonePreferenceQuery(teamId), enabled: Boolean(teamId) });
  const zone = teamId ? preference.data?.timezone : undefined;

  useEffect(() => {
    setDisplayTimeZone(zone);
    return () => setDisplayTimeZone(undefined);
  }, [zone]);

  return children;
}
