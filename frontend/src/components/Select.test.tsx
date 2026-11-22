import { act, fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { afterEach, beforeEach, expect, it, vi } from "vitest";
import { Select } from "./Select";

beforeEach(() => {
  vi.stubGlobal("ResizeObserver", class { observe = vi.fn(); disconnect = vi.fn(); });
});
afterEach(() => vi.unstubAllGlobals());

it("supports keyboard selection, skips disabled options and keeps native form values", async () => {
  const user = userEvent.setup();
  render(<form aria-label="Settings"><label htmlFor="region">Region</label><Select id="region" name="region" defaultValue="eu"><option value="eu">Europe</option><option value="disabled" disabled>Unavailable</option><option value="us">United States</option></Select></form>);
  const control = screen.getByRole("combobox", { name: "Region" });
  control.focus();
  await user.keyboard("{ArrowDown}{ArrowDown}{Enter}");
  expect(control).toHaveTextContent("United States");
  expect(control).toHaveFocus();
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  const form = screen.getByRole("form");
  if (!(form instanceof HTMLFormElement)) throw new Error("Expected form");
  expect(new FormData(form).get("region")).toBe("us");
  act(() => form.reset());
  expect(control).toHaveTextContent("Europe");
  expect(new FormData(form).get("region")).toBe("eu");
});

it("supports controlled values, pointer selection and typeahead", async () => {
  const user = userEvent.setup();
  function Example() {
    const [value, setValue] = useState("alpha");
    return <><Select aria-label="Resource" value={value} onChange={event => setValue(event.target.value)}><option value="alpha">Alpha</option><option value="beta">Beta</option><option value="gamma">Gamma</option></Select><output>{value}</output></>;
  }
  render(<Example />);
  const control = screen.getByRole("combobox", { name: "Resource" });
  await user.click(control);
  await user.click(screen.getByRole("option", { name: "Beta" }));
  expect(screen.getByRole("status")).toHaveTextContent("beta");
  await user.keyboard("g");
  expect(control).toHaveTextContent("Gamma");
  expect(screen.getByRole("status")).toHaveTextContent("gamma");
});

it("cancels navigation on Escape and closes on outside click without changing selection", async () => {
  const user = userEvent.setup();
  render(<><Select aria-label="Choice"><option>First</option><option>Second</option></Select><button>Outside</button></>);
  const control = screen.getByRole("combobox");
  await user.click(control);
  await user.keyboard("{End}{Escape}");
  expect(control).toHaveTextContent("First");
  expect(control).toHaveFocus();
  await user.click(control);
  await user.click(screen.getByRole("button", { name: "Outside" }));
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
});

it("preserves required validation and focuses the visible control", async () => {
  const user = userEvent.setup();
  const submit = vi.fn();
  render(<form onSubmit={event => { event.preventDefault(); submit(); }}><label htmlFor="required">Node</label><Select id="required" name="node" required><option value="">Choose a node</option><option value="one">Node one</option></Select><button type="submit">Save</button></form>);
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(submit).not.toHaveBeenCalled();
  const control = screen.getByRole("combobox", { name: "Node" });
  expect(control).toHaveFocus();
  expect(control).toHaveAttribute("aria-invalid", "true");
  await user.click(control);
  await user.click(screen.getByRole("option", { name: "Node one" }));
  await user.click(screen.getByRole("button", { name: "Save" }));
  expect(submit).toHaveBeenCalledOnce();
});

it("handles option groups and updated options without selecting disabled entries", async () => {
  const user = userEvent.setup();
  const { rerender } = render(<Select aria-label="Environment" value="one"><optgroup label="Project"><option value="one">Production</option><option value="two">Staging</option></optgroup></Select>);
  rerender(<Select aria-label="Environment" value="two"><optgroup label="Project"><option value="one">Production</option><option value="two">Staging</option></optgroup><optgroup label="Unavailable" disabled><option value="three">Archived</option></optgroup></Select>);
  const control = screen.getByRole("combobox");
  expect(control).toHaveTextContent("Staging");
  await user.click(control);
  expect(screen.getByRole("option", { name: "Archived" })).toHaveAttribute("aria-disabled", "true");
  await user.click(screen.getByRole("option", { name: "Archived" }));
  expect(control).toHaveTextContent("Staging");
  expect(control).toHaveAttribute("aria-expanded", "true");
});

it("keeps a dialog's popup inside its modal layer and respects disabled fieldsets", async () => {
  const user = userEvent.setup();
  render(<dialog open aria-label="Setup"><Select aria-label="Location"><option>Local</option><option>Remote</option></Select><fieldset disabled><Select aria-label="Disabled"><option>One</option></Select></fieldset></dialog>);
  await user.click(screen.getByRole("combobox", { name: "Disabled" }));
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Location" }));
  expect(screen.getByRole("dialog")).toContainElement(screen.getByRole("listbox"));
  const escape = new KeyboardEvent("keydown", { key: "Escape", bubbles: true, cancelable: true });
  fireEvent(screen.getByRole("combobox", { name: "Location" }), escape);
  expect(escape.defaultPrevented).toBe(true);
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  await user.click(screen.getByRole("combobox", { name: "Location" }));
  fireEvent.keyDown(screen.getByRole("combobox", { name: "Location" }), { key: "Tab" });
  expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
});
