import { MutationCache, QueryCache, QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { RouterProvider } from "@tanstack/react-router";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { ApiError } from "./api/client";
import { keys } from "./api/queries";
import { captureFragmentToken } from "./lib/fragment";
import { rememberTeam } from "./lib/team";
import { createAppRouter } from "./router";
import "./index.css";

function handleUnauthorized(error: unknown) {
  if (error instanceof ApiError && error.status === 401 && queryClient.getQueryData(keys.session)) {
    // Nothing cached under the expired session, including the selected team, may carry into the next sign-in.
    rememberTeam(null);
    void queryClient.cancelQueries();
    queryClient.clear();
    queryClient.setQueryData(keys.session, null);
    void router.navigate({ to: "/login", search: { redirect: router.state.location.href } });
  }
}

const queryClient = new QueryClient({
  queryCache: new QueryCache({ onError: handleUnauthorized }),
  // Some account flows answer 401 for a rejected ceremony or code while the session itself is still valid.
  mutationCache: new MutationCache({ onError: (error, _variables, _context, mutation) => { if (!mutation.meta?.sessionSurvives401) handleUnauthorized(error); } }),
  defaultOptions: {
    queries: {
      staleTime: 5_000,
      retry: (failureCount, error) => {
        if (error instanceof ApiError && error.status >= 400 && error.status < 500) return false;
        return failureCount < 2;
      },
    },
    // Fail immediately while offline instead of replaying deploys or enrollments later.
    mutations: { networkMode: "always", retry: false },
  },
});

captureFragmentToken(window.location, window.history);
const router = createAppRouter(queryClient);

const root = document.getElementById("root");
if (!root) throw new Error("Missing #root element");

createRoot(root).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
