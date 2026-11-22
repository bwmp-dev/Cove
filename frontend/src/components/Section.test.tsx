import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { expect, it } from "vitest";
import { Section } from "./ui";

it("keeps collapsed settings mounted behind a summary and toggles them from the heading", async () => {
  const user = userEvent.setup();
  render(
    <Section title="Deployment policy" description="Rules for deployments." collapsed={{ summary: "Approval required · Any time of day" }}>
      <label>
        Freeze starts
        <input defaultValue="draft" />
      </label>
    </Section>,
  );
  const toggle = screen.getByRole("button", { name: "Deployment policy" });
  expect(screen.getByRole("heading", { name: "Deployment policy" })).toContainElement(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "false");
  expect(screen.getByText("Approval required · Any time of day")).toBeVisible();
  expect(screen.getByLabelText("Freeze starts")).not.toBeVisible();

  await user.click(toggle);
  expect(toggle).toHaveAttribute("aria-expanded", "true");
  expect(screen.getByLabelText("Freeze starts")).toBeVisible();
  expect(screen.getByText("Rules for deployments.")).toBeVisible();
  expect(screen.queryByText("Approval required · Any time of day")).not.toBeInTheDocument();

  await user.type(screen.getByLabelText("Freeze starts"), " edit");
  await user.click(toggle);
  await user.click(toggle);
  expect(screen.getByLabelText("Freeze starts")).toHaveValue("draft edit");
});
