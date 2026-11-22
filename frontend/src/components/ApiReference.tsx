import { useEffect } from "react";
import SwaggerUI from "swagger-ui-react";
import "swagger-ui-react/swagger-ui.css";
import "./api-reference.css";
import { selectedTeam } from "../lib/team";

interface InterceptedRequest {
  headers?: Record<string, string>;
  credentials?: string;
}

// Try it out uses the dashboard's HttpOnly session cookie in the selected team. The browser never sends a bearer
// credential, so the authorization controls are removed instead of offering a field for one.
const sessionOnly = () => ({
  wrapComponents: {
    AuthorizeBtnContainer: () => () => null,
    authorizeOperationBtn: () => () => null,
  },
});

function requestInterceptor<T extends InterceptedRequest>(request: T): T {
  const headers = { ...request.headers };
  delete headers.Authorization;
  delete headers.authorization;
  const team = selectedTeam();
  if (team && !headers["X-Cove-Team"]) headers["X-Cove-Team"] = team;
  return { ...request, headers, credentials: "same-origin" };
}

export function ApiReference({ spec }: { spec: object }) {
  // Swagger UI's dark theme is keyed to a class on the root element; keep it only while the reference is shown.
  useEffect(() => {
    const root = document.documentElement;
    root.classList.add("dark-mode");
    return () => root.classList.remove("dark-mode");
  }, []);
  return (
    <div className="api-reference">
      <SwaggerUI
        spec={spec}
        plugins={[sessionOnly]}
        requestInterceptor={requestInterceptor}
        docExpansion="list"
        defaultModelsExpandDepth={0}
        showExtensions
        displayRequestDuration
        persistAuthorization={false}
        queryConfigEnabled={false}
        deepLinking={false}
        filter
      />
    </div>
  );
}
