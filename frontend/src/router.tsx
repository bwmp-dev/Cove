import type { QueryClient } from "@tanstack/react-query";
import { createRootRouteWithContext, createRoute, createRouter, lazyRouteComponent, Outlet, redirect } from "@tanstack/react-router";
import { sessionQuery } from "./api/queries";
import { ApiError } from "./api/client";
import { AppShell } from "./components/AppShell";
import { NotFound, RouteError } from "./components/RouteStates";
import { parseOperationsSearch } from "./lib/operations";
import { safeRedirect } from "./lib/redirect";
import { rememberTeam, selectedTeam } from "./lib/team";

interface RouterContext {
  queryClient: QueryClient;
}

const rootRoute = createRootRouteWithContext<RouterContext>()({
  component: Outlet,
  notFoundComponent: NotFound,
  errorComponent: RouteError,
});

const loginRoute = createRoute({
  getParentRoute: () => rootRoute,
  path: "/login",
  validateSearch: (search: Record<string, unknown>): { redirect?: string; error?: string } => ({
    ...(typeof search.redirect === "string" ? { redirect: search.redirect } : {}),
    ...(typeof search.error === "string" ? { error: search.error } : {}),
  }),
  beforeLoad: async ({ context, search }) => {
    const session = await context.queryClient.ensureQueryData(sessionQuery).catch(() => null);
    if (session) {
      throw redirect({ href: safeRedirect(search.redirect) });
    }
  },
  component: lazyRouteComponent(() => import("./routes/login"), "LoginPage"),
});

const registerRoute = createRoute({ getParentRoute: () => rootRoute, path: "/register", component: lazyRouteComponent(() => import("./routes/auth"), "RegisterPage") });
const verifyRoute = createRoute({ getParentRoute: () => rootRoute, path: "/verify", component: lazyRouteComponent(() => import("./routes/auth"), "VerifyEmailPage") });
const forgotRoute = createRoute({ getParentRoute: () => rootRoute, path: "/forgot-password", component: lazyRouteComponent(() => import("./routes/auth"), "ForgotPasswordPage") });
const resetRoute = createRoute({ getParentRoute: () => rootRoute, path: "/reset-password", component: lazyRouteComponent(() => import("./routes/auth"), "ResetPasswordPage") });
const inviteRoute = createRoute({ getParentRoute: () => rootRoute, path: "/invite", component: lazyRouteComponent(() => import("./routes/auth"), "InvitationPage") });
// Public project status pages sit outside the authenticated shell and never read the session.
const statusRoute = createRoute({ getParentRoute: () => rootRoute, path: "/status/$slug", component: lazyRouteComponent(() => import("./routes/status"), "PublicStatusPage") });

const appRoute = createRoute({
  getParentRoute: () => rootRoute,
  id: "app",
  beforeLoad: async ({ context, location }) => {
    const session = await context.queryClient.ensureQueryData(sessionQuery).catch((error: unknown) => {
      if (error instanceof ApiError && error.status === 401) return null;
      throw error;
    });
    if (!session) {
      throw redirect({ to: "/login", search: { redirect: location.href } });
    }
    // Follow the server when a remembered team is no longer available to this account.
    if (session.team_id !== selectedTeam()) {
      rememberTeam(session.team_id || null);
    }
  },
  component: AppShell,
  errorComponent: RouteError,
});

const indexRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/",
  beforeLoad: () => {
    throw redirect({ to: "/dashboard" });
  },
});

const terminalRoute = createRoute({ getParentRoute: () => appRoute, path: "/terminal", component: lazyRouteComponent(() => import("./routes/terminal"), "TerminalPage") });
const dashboardRoute = createRoute({ getParentRoute: () => appRoute, path: "/dashboard", component: lazyRouteComponent(() => import("./routes/dashboard"), "DashboardPage") });
const integrationsRoute = createRoute({ getParentRoute: () => appRoute, path: "/integrations", component: lazyRouteComponent(() => import("./routes/integrations"), "IntegrationsPage") });
const registriesRoute = createRoute({ getParentRoute: () => appRoute, path: "/registries", component: lazyRouteComponent(() => import("./routes/registries"), "RegistriesPage") });
const storageRoute = createRoute({ getParentRoute: () => appRoute, path: "/storage", component: lazyRouteComponent(() => import("./routes/storage"), "StoragePage") });
const notificationsRoute = createRoute({ getParentRoute: () => appRoute, path: "/notifications", component: lazyRouteComponent(() => import("./routes/notifications"), "NotificationsPage") });
const usersRoute = createRoute({ getParentRoute: () => appRoute, path: "/users", component: lazyRouteComponent(() => import("./routes/admin"), "UsersPage") });
const projectsRoute = createRoute({ getParentRoute: () => appRoute, path: "/projects", component: lazyRouteComponent(() => import("./routes/projects"), "ProjectsPage") });
const projectRoute = createRoute({ getParentRoute: () => appRoute, path: "/projects/$projectId", component: lazyRouteComponent(() => import("./routes/project"), "ProjectLayout") });
const projectOverviewRoute = createRoute({ getParentRoute: () => projectRoute, path: "/", component: lazyRouteComponent(() => import("./routes/project"), "ProjectOverview") });
const environmentRoute = createRoute({ getParentRoute: () => projectRoute, path: "/environments/$environmentId", component: lazyRouteComponent(() => import("./routes/project"), "EnvironmentPage") });
const projectPreviewsRoute = createRoute({ getParentRoute: () => projectRoute, path: "/previews", component: lazyRouteComponent(() => import("./routes/project-previews"), "ProjectPreviews") });

const resourcesRoute = createRoute({ getParentRoute: () => appRoute, path: "/resources", component: lazyRouteComponent(() => import("./routes/workloads"), "WorkloadsPage") });
const resourceNewRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/resources/new",
  validateSearch: (search: Record<string, unknown>): { project?: string; environment?: string } => ({
    project: typeof search.project === "string" ? search.project : undefined,
    environment: typeof search.environment === "string" ? search.environment : undefined,
  }),
  component: lazyRouteComponent(() => import("./routes/workload-new"), "NewWorkloadPage"),
});
const resourceRoute = createRoute({ getParentRoute: () => appRoute, path: "/resources/$resourceId", component: lazyRouteComponent(() => import("./routes/workload-detail"), "WorkloadPage") });
const resourceOverviewRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceOverview") });
const resourceLogsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/logs", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceLogs") });
const resourceMetricsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/metrics", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceMetrics") });
const resourceOperationsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/operations", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceOperations") });
// Configuration used to be one page; keep its address working for saved links.
const resourceConfigurationRoute = createRoute({
  getParentRoute: () => resourceRoute,
  path: "/configuration",
  beforeLoad: ({ params }) => {
    throw redirect({ to: "/resources/$resourceId/general", params });
  },
});
const resourcePreflightRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/preflight", component: lazyRouteComponent(() => import("./routes/preflight"), "ResourcePreflight") });
const resourceSafetyRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/deployment-safety", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceDeploymentSafety") });
const resourcePreviewsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/previews", component: lazyRouteComponent(() => import("./routes/previews"), "Previews") });
const resourceHooksRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/hooks", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceHooks") });
const resourceScheduleRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/schedule", component: lazyRouteComponent(() => import("./routes/job-schedule"), "JobSchedule") });
const resourcePromotionRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/promotion", component: lazyRouteComponent(() => import("./routes/promotion"), "Promotion") });
const resourceTroubleshootingRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/troubleshooting", component: lazyRouteComponent(() => import("./routes/troubleshooting"), "Troubleshooting") });
const resourceHistoryRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/configuration-history", component: lazyRouteComponent(() => import("./routes/configuration-history"), "ConfigurationHistory") });
const resourceLimitsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/limits", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceLimits") });
const resourceGeneralRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/general", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceGeneral") });
const resourceSourceRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/source", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceSource") });
const resourceDomainsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/domains", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceDomains") });
const resourceEnvironmentRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/environment", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceEnvironment") });
const resourceNetworkingRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/networking", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceNetworking") });
const resourceRegistryRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/registry", component: lazyRouteComponent(() => import("./routes/resource-config"), "ResourceRegistryPage") });
const resourceBackupsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/backups", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceBackups") });
const resourceFilesRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/files", component: lazyRouteComponent(() => import("./routes/resource-files"), "ResourceFiles") });
const resourceSettingsRoute = createRoute({ getParentRoute: () => resourceRoute, path: "/settings", component: lazyRouteComponent(() => import("./routes/workload-detail"), "ResourceSettings") });

const nodesRoute = createRoute({ getParentRoute: () => appRoute, path: "/nodes", component: lazyRouteComponent(() => import("./routes/nodes"), "NodesPage") });
const nodeNewRoute = createRoute({ getParentRoute: () => appRoute, path: "/nodes/new", component: lazyRouteComponent(() => import("./routes/node-new"), "NewNodePage") });
const nodeRoute = createRoute({ getParentRoute: () => appRoute, path: "/nodes/$nodeId", component: lazyRouteComponent(() => import("./routes/node-detail"), "NodePage") });
const nodeProxyRoute = createRoute({ getParentRoute: () => appRoute, path: "/nodes/$nodeId/proxy", component: lazyRouteComponent(() => import("./routes/node-proxy"), "NodeProxyPage") });
const operationsRoute = createRoute({
  getParentRoute: () => appRoute,
  path: "/operations",
  validateSearch: parseOperationsSearch,
  component: lazyRouteComponent(() => import("./routes/operations"), "OperationsPage"),
});
const operationRoute = createRoute({ getParentRoute: () => appRoute, path: "/operations/$operationId", component: lazyRouteComponent(() => import("./routes/operation-detail"), "OperationPage") });
const tokensRoute = createRoute({ getParentRoute: () => appRoute, path: "/tokens", component: lazyRouteComponent(() => import("./routes/tokens"), "TokensPage") });
const accountRoute = createRoute({ getParentRoute: () => appRoute, path: "/account", component: lazyRouteComponent(() => import("./routes/account"), "AccountPage") });
const teamRoute = createRoute({ getParentRoute: () => appRoute, path: "/team", component: lazyRouteComponent(() => import("./routes/team"), "TeamPage") });
const adminRoute = createRoute({ getParentRoute: () => appRoute, path: "/admin", component: lazyRouteComponent(() => import("./routes/admin"), "AdminPage") });
const auditRoute = createRoute({ getParentRoute: () => appRoute, path: "/audit", component: lazyRouteComponent(() => import("./routes/audit"), "AuditPage") });

const routeTree = rootRoute.addChildren([
  loginRoute,
  registerRoute,
  verifyRoute,
  forgotRoute,
  resetRoute,
  inviteRoute,
  statusRoute,
  appRoute.addChildren([
    indexRoute,
    dashboardRoute,
    terminalRoute,
    integrationsRoute,
    usersRoute,
    registriesRoute,
    storageRoute,
    notificationsRoute,
    projectsRoute,
    projectRoute.addChildren([projectOverviewRoute, environmentRoute, projectPreviewsRoute]),
    resourcesRoute,
    resourceNewRoute,
    resourceRoute.addChildren([
      resourceOverviewRoute,
      resourceLogsRoute,
      resourceMetricsRoute,
      resourceOperationsRoute,
      resourceConfigurationRoute,
      resourceGeneralRoute,
      resourceLimitsRoute,
      resourceHistoryRoute,
      resourcePreflightRoute,
      resourceSourceRoute,
      resourceDomainsRoute,
      resourceEnvironmentRoute,
      resourceScheduleRoute,
      resourceHooksRoute,
      resourceSafetyRoute,
      resourcePreviewsRoute,
      resourcePromotionRoute,
      resourceTroubleshootingRoute,
      resourceNetworkingRoute,
      resourceRegistryRoute,
      resourceBackupsRoute,
      resourceFilesRoute,
      resourceSettingsRoute,
    ]),
    nodesRoute,
    nodeNewRoute,
    nodeRoute,
    nodeProxyRoute,
    operationsRoute,
    operationRoute,
    tokensRoute,
    accountRoute,
    teamRoute,
    adminRoute,
    auditRoute,
  ]),
]);

export function createAppRouter(queryClient: QueryClient) {
  return createRouter({
    routeTree,
    context: { queryClient },
    defaultPreload: "intent",
    defaultPreloadStaleTime: 0,
    scrollRestoration: true,
  });
}

declare module "@tanstack/react-router" {
  interface Register {
    router: ReturnType<typeof createAppRouter>;
  }
}
