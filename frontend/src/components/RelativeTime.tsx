import { formatAbsolute, formatRelative } from "../lib/format";
import { useNow } from "../lib/hooks";

export function RelativeTime({ value, className = "" }: { value: string; className?: string }) {
  const now = useNow(15_000);
  return (
    <time dateTime={value} title={formatAbsolute(value)} className={`whitespace-nowrap ${className}`}>
      {formatRelative(value, now)}
    </time>
  );
}

// Reads "Setup command expires in 5 minutes" or "... expired 5 minutes ago" so past deadlines are not phrased as future ones.
export function ExpiryTime({ value, subject }: { value: string; subject?: string }) {
  const now = useNow(15_000);
  const verb = Date.parse(value) <= now ? "expired" : "expires";
  return (
    <span>
      {subject ? `${subject} ${verb}` : verb.charAt(0).toUpperCase() + verb.slice(1)}{" "}
      <time dateTime={value} title={formatAbsolute(value)} className="whitespace-nowrap">
        {formatRelative(value, now)}
      </time>
    </span>
  );
}
