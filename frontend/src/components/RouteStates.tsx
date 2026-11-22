import { Link, useRouter, type ErrorComponentProps } from "@tanstack/react-router";
import { errorMessage } from "../api/client";
import { ErrorRemediation } from "./ErrorRemediation";
import { Button } from "./ui";
import { buttonClass } from "../lib/styles";

export function RouteError({ error, reset }: ErrorComponentProps) {
  const router = useRouter();
  return (
    <div className="mx-auto max-w-lg px-4 py-24">
      <h1 className="text-base font-semibold text-fg">Cove could not load this page</h1>
      <p role="alert" className="mt-2 text-[13px] text-fg-muted">
        {errorMessage(error)}
      </p>
      <ErrorRemediation error={error} />
      <Button
        variant="secondary"
        className="mt-5"
        onClick={() => {
          reset();
          void router.invalidate();
        }}
      >
        Try again
      </Button>
    </div>
  );
}

export function NotFound() {
  return (
    <div className="mx-auto max-w-lg px-4 py-24">
      <h1 className="text-base font-semibold text-fg">Page not found</h1>
      <p className="mt-2 text-[13px] text-fg-muted">The address does not match any page in Cove.</p>
      <Link to="/projects" className={buttonClass("secondary", "mt-5")}>
        Go to projects
      </Link>
    </div>
  );
}
