import qrcode from "qrcode-generator";
import { useMemo } from "react";

// Dark modules on a light field with a four-module quiet zone, which authenticator apps scan reliably.
export function QrCode({ value, label }: { value: string; label: string }) {
  const { size, path } = useMemo(() => {
    const code = qrcode(0, "M");
    code.addData(value);
    code.make();
    const count = code.getModuleCount();
    let modules = "";
    for (let row = 0; row < count; row += 1) {
      for (let col = 0; col < count; col += 1) {
        if (code.isDark(row, col)) modules += `M${col + 4} ${row + 4}h1v1h-1z`;
      }
    }
    return { size: count + 8, path: modules };
  }, [value]);
  return (
    <svg role="img" aria-label={label} viewBox={`0 0 ${size} ${size}`} shapeRendering="crispEdges" className="size-48 shrink-0 rounded-md">
      <rect width={size} height={size} className="fill-fg" />
      <path d={path} className="fill-canvas" />
    </svg>
  );
}
