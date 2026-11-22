import { Check, Copy } from "lucide-react";
import { useEffect, useState } from "react";
import { Button } from "./ui";

type Status = "idle" | "copied" | "failed";

export function CopyButton({ text, label = "Copy", variant = "secondary" }: { text: string; label?: string; variant?: "secondary" | "primary" | "ghost" }) {
  const [status, setStatus] = useState<Status>("idle");

  useEffect(() => {
    if (status === "idle") return;
    const timer = window.setTimeout(() => setStatus("idle"), 2500);
    return () => window.clearTimeout(timer);
  }, [status]);

  async function copy() {
    try {
      await navigator.clipboard.writeText(text);
      setStatus("copied");
    } catch {
      setStatus("failed");
    }
  }

  return (
    <span className="inline-flex items-center gap-2">
      <Button variant={variant} onClick={() => void copy()}>
        {status === "copied" ? <Check aria-hidden className="size-3.5" /> : <Copy aria-hidden className="size-3.5" />}
        {status === "copied" ? "Copied" : label}
      </Button>
      <span role="status" className="text-xs text-fg-muted">
        {status === "failed" ? "Clipboard unavailable. Select the text and copy it manually." : ""}
      </span>
    </span>
  );
}
