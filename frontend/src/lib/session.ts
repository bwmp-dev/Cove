import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "@tanstack/react-router";
import { sessionQuery } from "../api/queries";
import { currentTeam } from "./permissions";
import { rememberTeam } from "./team";

// Inside the authenticated layout the session is always loaded before rendering.
export function useSession() {
  const { data } = useQuery(sessionQuery);
  return data ?? null;
}

export function useCurrentTeam() {
  return currentTeam(useSession());
}

// Every cached query and mutation belongs to the previous team, including results of forms that carried secrets.
export function useSwitchTeam() {
  const client = useQueryClient();
  const navigate = useNavigate();
  return (teamId: string) => {
    rememberTeam(teamId);
    void client.cancelQueries();
    client.clear();
    void navigate({ to: "/projects" });
  };
}
