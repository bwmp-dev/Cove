import { useQuery } from "@tanstack/react-query";
import { errorMessage, request } from "../api/client";
import { Button, Notice, Section } from "./ui";
interface Preview { managed: boolean; image_bytes: number; sampled_at: string; images: { id: string; created_at: string; size: number }[] }

export function CleanupPreview({ nodeId }: { nodeId: string }) {
 const preview = useQuery({ queryKey: ["cleanup-preview", nodeId], queryFn: ({ signal }) => request<Preview>("GET", `/api/nodes/${nodeId}/cleanup-preview`, undefined, signal), enabled: false });
 return <Section title="Docker cleanup" description="Automatic cleanup runs every six hours. It considers only Cove-owned dangling build images older than seven days, and the private Cove builder cache older than seven days with a 2 GB retention target.">
  <div className="space-y-3">
   <p className="text-sm text-fg-muted">Tagged rollback images, images used by containers, volumes, networks, and other tools' images are retained. This preview makes no changes. Upgrade and verify the worker to configure the cleanup schedule and run reviewed cleanups.</p>
   <Button pending={preview.isFetching} onClick={() => void preview.refetch()}>Preview image cleanup</Button>
   {preview.isError && <Notice tone="error">{errorMessage(preview.error)}</Notice>}
   {preview.data && !preview.isError && <><p className="text-sm">{preview.data.managed ? `${preview.data.images.length} eligible images · ${(preview.data.image_bytes / 1024 / 1024).toFixed(1)} MiB total image size` : "No Cove-owned builder has been created on this worker."}</p><p className="text-xs text-fg-muted">{new Date(preview.data.sampled_at).toLocaleString()}. Shared layers mean actual reclaimed space may be smaller. Builder cache is not included in this estimate. Eligibility is rechecked by Docker when cleanup runs.</p><ul className="divide-y divide-line">{preview.data.images.map(image => <li key={image.id} className="flex flex-wrap justify-between gap-2 py-2 text-xs"><code className="break-all">{image.id}</code><span>{(image.size / 1024 / 1024).toFixed(1)} MiB</span></li>)}</ul></>}
  </div>
 </Section>;
}
