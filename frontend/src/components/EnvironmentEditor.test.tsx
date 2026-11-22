import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { useState } from "react";
import { describe, expect, it } from "vitest";
import { createRow, type EnvRow } from "../lib/workload-form";
import { EnvironmentEditor } from "./EnvironmentEditor";

function Harness({ initial }: { initial: EnvRow[] }) {
  const [rows, setRows] = useState(initial);
  return <EnvironmentEditor rows={rows} onChange={setRows} />;
}

describe("EnvironmentEditor", () => {
  it("masks values until deliberately revealed", async () => {
    render(<Harness initial={[createRow("API_KEY", "s3cret")]} />);
    const value = screen.getByLabelText("Value of API_KEY");
    expect(value).toHaveAttribute("type", "password");
    await userEvent.click(screen.getByRole("button", { name: "Show value of API_KEY" }));
    expect(value).toHaveAttribute("type", "text");
    await userEvent.click(screen.getByRole("button", { name: "Hide value of API_KEY" }));
    expect(value).toHaveAttribute("type", "password");
  });

  it("expands a pasted .env file into rows", async () => {
    render(<Harness initial={[createRow()]} />);
    await userEvent.click(screen.getByLabelText("Name of variable 1"));
    await userEvent.paste("A=1\nB=2");
    expect(screen.getByLabelText("Value of A")).toHaveValue("1");
    expect(screen.getByLabelText("Value of B")).toHaveValue("2");
  });

  it("adds and removes variables", async () => {
    render(<Harness initial={[]} />);
    await userEvent.click(screen.getByRole("button", { name: "Add variable" }));
    expect(screen.getByLabelText("Name of variable 1")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Remove variable 1" }));
    expect(screen.queryByLabelText("Name of variable 1")).not.toBeInTheDocument();
  });
});
