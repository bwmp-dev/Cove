import { render, screen } from "@testing-library/react";
import { expect, it } from "vitest";
import { ExpiryTime } from "./RelativeTime";

it("phrases past and future deadlines in the matching tense", () => {
  const hour = 60 * 60 * 1000;
  const { rerender } = render(<ExpiryTime subject="Setup command" value={new Date(Date.now() + 2 * hour).toISOString()} />);
  expect(screen.getByText(/^Setup command expires/)).toHaveTextContent("Setup command expires in 2 hours");
  rerender(<ExpiryTime subject="Setup command" value={new Date(Date.now() - 2 * hour).toISOString()} />);
  expect(screen.getByText(/^Setup command expired/)).toHaveTextContent("Setup command expired 2 hours ago");
  rerender(<ExpiryTime value={new Date(Date.now() - 2 * hour).toISOString()} />);
  expect(screen.getByText(/^Expired/)).toBeInTheDocument();
});
