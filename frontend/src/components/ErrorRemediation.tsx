import { useRouter } from "@tanstack/react-router";
import type { MouseEvent } from "react";
import { stripBasePath, withBasePath } from "../lib/base-path";
import { remediationFor, type Remediation } from "../lib/error-remediation";
import { linkClass } from "../lib/styles";

// A suggested fix shown under an error's original text. The app uses browser history, so the
// current address is the router's location; unknown errors render nothing and touch no router state.
export function ErrorRemediation({ error }: { error: unknown }) {
  const remediation = remediationFor(error, stripBasePath(window.location.pathname));
  if (!remediation) return null;
  return (
    <p className="mt-1 text-xs text-fg-muted">
      {remediation.fix}
      {remediation.link && (
        <>
          {" "}
          <RemediationLink link={remediation.link} />
        </>
      )}
    </p>
  );
}

// Shared error states also render outside a router (component tests, the root error boundary),
// where the link falls back to a plain same-origin address.
function RemediationLink({ link }: { link: NonNullable<Remediation["link"]> }) {
  const router = useRouter({ warn: false }) as ReturnType<typeof useRouter> | undefined;
  const href = withBasePath(link.href);
  const navigate = (event: MouseEvent<HTMLAnchorElement>) => {
    if (!router || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
    event.preventDefault();
    void router.navigate({ href });
  };
  return (
    <a href={href} onClick={navigate} className={linkClass}>
      {link.label}
    </a>
  );
}
