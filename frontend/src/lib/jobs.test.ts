import { expect, it } from "vitest";
import { formatTimeout, hasAdvancedExecution, jobOutcome, parseJobTimeout } from "./jobs";

it("reports only the exit status the worker returned", () => {
  expect(jobOutcome({ exit_code: 0, timed_out: false, containers_stopped: true })).toMatchObject({ exit: "Exited with code 0.", succeeded: true, logs: undefined });
  expect(jobOutcome({ exit_code: 3, timed_out: false, containers_stopped: true })).toMatchObject({ exit: "Exited with code 3.", succeeded: false });
  expect(jobOutcome({ timed_out: false, containers_stopped: true })).toMatchObject({ exit: "Exit code unknown.", succeeded: false });
  expect(jobOutcome({ exit_code: 137, timed_out: true, containers_stopped: true })).toMatchObject({ exit: "Execution timed out (exit code 137).", succeeded: false });
  const unconfirmed = jobOutcome({ exit_code: 0, timed_out: false, containers_stopped: false });
  expect(unconfirmed.succeeded).toBe(false);
  expect(unconfirmed.cleanup).toMatch(/^Cleanup could not be confirmed/);
  const timedOut = jobOutcome({ timed_out: true, containers_stopped: false });
  expect(timedOut.exit).toBe("Execution timed out.");
  expect(timedOut.cleanup).toMatch(/^Cleanup could not be confirmed/);
  expect(jobOutcome({ exit_code: 0, timed_out: false, containers_stopped: true, logs_unavailable: true }).logs).toBe("Output could not be collected from the worker.");
});

it("parses timeouts within the worker limits", () => {
  expect(parseJobTimeout(" 5 ")).toEqual({ value: 5 });
  expect(parseJobTimeout("86400")).toEqual({ value: 86400 });
  expect(parseJobTimeout("4").error).toBe("Use 5 to 86,400 seconds.");
  expect(parseJobTimeout("86401").error).toBe("Use 5 to 86,400 seconds.");
  expect(parseJobTimeout("-10").error).toBe("Use a whole number of seconds.");
  expect(parseJobTimeout("").error).toBe("Enter a timeout in seconds.");
});

it("treats only non-default execution settings as advanced", () => {
  expect(hasAdvancedExecution("skip", 1800)).toBe(false);
  expect(hasAdvancedExecution("queue_one", 1800)).toBe(true);
  expect(hasAdvancedExecution("skip", 60)).toBe(true);
});

it("formats timeouts in words", () => {
  expect(formatTimeout(1800)).toBe("30 minutes");
  expect(formatTimeout(3661)).toBe("1 hour 1 minute 1 second");
  expect(formatTimeout(5)).toBe("5 seconds");
});
