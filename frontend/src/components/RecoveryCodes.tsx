import { Download } from "lucide-react";
import { useId, useState } from "react";
import { Checkbox } from "./Checkbox";
import { CopyButton } from "./CopyButton";
import { Button, Notice } from "./ui";

export function recoveryCodesFile(codes: readonly string[], account: string) {
  return [`Cove recovery codes for ${account}`, "Each code works once. Keep this file somewhere safe and offline.", "", ...codes, ""].join("\n");
}

function download(codes: readonly string[], account: string) {
  const url = URL.createObjectURL(new Blob([recoveryCodesFile(codes, account)], { type: "text/plain" }));
  const link = document.createElement("a");
  link.href = url;
  link.download = "cove-recovery-codes.txt";
  link.click();
  URL.revokeObjectURL(url);
}

// The codes exist only in this component's props; closing it drops them for good.
export function RecoveryCodes({ codes, account, onDone }: { codes: readonly string[]; account: string; onDone: () => void }) {
  const id = useId();
  const [saved, setSaved] = useState(false);
  return (
    <div className="space-y-4">
      <Notice tone="warn" title="Save your recovery codes now">
        Each code signs you in once if you lose your authenticator. They are shown only this once and cannot be retrieved later.
      </Notice>
      <ol aria-label="Recovery codes" className="grid gap-x-6 gap-y-1.5 rounded-md border border-line bg-panel p-3 font-mono text-[13px] text-fg sm:grid-cols-2">
        {codes.map((code) => (
          <li key={code} className="break-all">
            {code}
          </li>
        ))}
      </ol>
      <div className="flex flex-wrap items-center gap-2">
        <CopyButton text={codes.join("\n")} label="Copy codes" />
        <Button onClick={() => download(codes, account)}>
          <Download aria-hidden className="size-3.5" />
          Download as text file
        </Button>
      </div>
      <div className="flex flex-wrap items-center justify-between gap-3 border-t border-line pt-4">
        <label htmlFor={`${id}-saved`} className="flex items-center gap-2.5 text-[13px] text-fg">
          <Checkbox id={`${id}-saved`} checked={saved} onChange={(event) => setSaved(event.target.checked)} />I have saved these codes
        </label>
        <Button variant="primary" disabled={!saved} onClick={onDone}>
          Done
        </Button>
      </div>
    </div>
  );
}
