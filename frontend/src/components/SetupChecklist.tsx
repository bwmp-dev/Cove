import { useQuery } from "@tanstack/react-query";
import { Link } from "@tanstack/react-router";
import { CheckCircle2, Circle } from "lucide-react";
import { errorMessage } from "../api/client";
import { onboardingQuery, useDismissOnboarding, type Onboarding, type OnboardingStep } from "../api/organization";
import { buttonClass } from "../lib/styles";
import { Button, Notice, QueryError, Section } from "./ui";

// Only the dashboard's own static pages are linked; anything else the server sends is shown as text.
const stepRoutes = ["/account", "/nodes", "/projects", "/resources/new", "/resources"] as const;
type StepRoute = (typeof stepRoutes)[number];

function stepRoute(path: string): StepRoute | undefined {
  return stepRoutes.find((route) => route === path);
}

const stepHelp: Record<string, string> = {
  account: "You are signed in.",
  node: "Add a Linux worker and verify it over SSH with Docker Compose available.",
  project: "Projects hold resources directly or inside environments such as production.",
  resource: "Create a resource from a template, a Compose file or a Git repository.",
  deployment: "Open the resource and deploy it, or adopt containers already running on a node.",
};

function StepRow({ step, next, onboarding }: { step: OnboardingStep; next: boolean; onboarding: Onboarding }) {
  const route = stepRoute(step.path);
  const blocked = step.key === "node" && !step.complete && !onboarding.can_enroll_node;
  return (
    <li className="flex flex-wrap items-center gap-x-3 gap-y-2 px-3 py-2.5" aria-current={next ? "step" : undefined}>
      {step.complete ? <CheckCircle2 aria-hidden className="size-4 shrink-0 text-ok" /> : <Circle aria-hidden className="size-4 shrink-0 text-fg-subtle" />}
      <div className="min-w-0 flex-1 basis-48">
        <p className={`text-[13px] ${step.complete ? "text-fg-muted" : "font-medium text-fg"}`}>
          {step.name}
          <span className="sr-only">{step.complete ? ", done" : ", not done"}</span>
        </p>
        {!step.complete && (
          <p className="mt-0.5 text-xs text-fg-muted">
            {blocked ? "Only an instance administrator can enroll workers. Ask one to add a node and assign it to this team." : stepHelp[step.key]}
          </p>
        )}
      </div>
      {!step.complete && !blocked && route && (
        <Link to={route} className={buttonClass(next ? "primary" : "secondary", "", "sm")} aria-label={`${next ? "Start" : "Open"}: ${step.name}`}>
          {next ? "Start" : "Open"}
        </Link>
      )}
    </li>
  );
}

// Reads progress from real team state. It never deploys, enrolls or creates anything on its own.
export function SetupChecklist() {
  const onboarding = useQuery(onboardingQuery);
  const dismiss = useDismissOnboarding();
  if (onboarding.isPending) return null;
  if (onboarding.isError) {
    return (
      <Section title="Get started">
        <QueryError what="setup progress" error={onboarding.error} onRetry={() => void onboarding.refetch()} retrying={onboarding.isFetching} />
      </Section>
    );
  }
  const state = onboarding.data;
  const failure = dismiss.isError && (
    <div className="border-b border-line p-3">
      <Notice tone="error">{errorMessage(dismiss.error)}</Notice>
    </div>
  );
  if (state.dismissed) {
    if (state.complete) return null;
    return (
      <div className="flex flex-wrap items-center gap-x-3 gap-y-1 px-4 pt-5 text-xs text-fg-muted sm:px-6">
        <span>Setup checklist hidden.</span>
        <Button size="sm" variant="ghost" pending={dismiss.isPending} onClick={() => dismiss.mutate(false)}>
          Show setup checklist
        </Button>
        {dismiss.isError && (
          <span role="alert" className="text-bad">
            {errorMessage(dismiss.error)}
          </span>
        )}
      </div>
    );
  }
  const done = state.steps.filter((step) => step.complete).length;
  const nextKey = state.steps.find((step) => !step.complete)?.key;
  return (
    <Section
      title="Get started"
      description={state.complete ? "Your first app is deployed." : `${done} of ${state.steps.length} steps done. Progress follows what exists in this team.`}
      actions={
        <Button size="sm" variant="ghost" pending={dismiss.isPending} onClick={() => dismiss.mutate(true)}>
          {state.complete ? "Hide" : "Skip for now"}
        </Button>
      }
      flush
    >
      {failure}
      <ol aria-label="Setup steps" className="divide-y divide-line">
        {state.steps.map((step) => (
          <StepRow key={step.key} step={step} next={step.key === nextKey} onboarding={state} />
        ))}
      </ol>
    </Section>
  );
}
