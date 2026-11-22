import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { ActionMenu } from "./ActionMenu";

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class {
    observe = vi.fn();
    disconnect = vi.fn();
  });
});
afterEach(() => vi.unstubAllGlobals());

it("supports keyboard navigation, explains unavailable actions and returns focus", async () => {
  const user = userEvent.setup();
  const clone = vi.fn();
  const remove = vi.fn();
  render(
    <ActionMenu
      label="Project actions"
      actions={[
        { label: "Clone project", onSelect: clone },
        { label: "Delete project", onSelect: remove, danger: true, disabledReason: "Move or delete its 2 resources first." },
      ]}
    />,
  );
  const button = screen.getByRole("button", { name: "Project actions" });
  expect(button).toHaveAttribute("aria-haspopup", "menu");
  button.focus();
  await user.keyboard("{ArrowDown}");
  expect(button).toHaveAttribute("aria-expanded", "true");
  const items = await screen.findAllByRole("menuitem");
  await vi.waitFor(() => expect(items[0]).toHaveFocus());
  await user.keyboard("{ArrowDown}");
  expect(items[1]).toHaveFocus();
  expect(items[1]).toHaveAttribute("aria-disabled", "true");
  expect(items[1]).toHaveTextContent("Move or delete its 2 resources first.");
  await user.keyboard("{Enter}");
  expect(remove).not.toHaveBeenCalled();
  await user.keyboard("{ArrowDown}");
  expect(items[0]).toHaveFocus();
  await user.keyboard("{Escape}");
  expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  expect(button).toHaveFocus();

  await user.click(button);
  await user.click(await screen.findByRole("menuitem", { name: "Clone project" }));
  expect(clone).toHaveBeenCalledOnce();
  expect(screen.queryByRole("menu")).not.toBeInTheDocument();
  expect(button).toHaveFocus();
});
