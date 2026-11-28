import { Boxes, Database, Timer } from "lucide-react";

export function ResourceIcon({ kind, className }: { kind: string | undefined; className?: string }) {
  if (kind === "job") return <Timer aria-hidden className={className} />;
  if (kind === "database") return <Database aria-hidden className={className} />;
  return <Boxes aria-hidden className={className} />;
}
