import { HardDrive } from "lucide-react";
import { Connections } from "../components/Connections";
import { PageHeader } from "../components/ui";
export function StoragePage() { return <><PageHeader icon={<HardDrive aria-hidden />} title="Storage locations" description="Reusable S3, Backblaze B2, SFTP, FTPS, and WebDAV destinations. Select a location in a resource’s backup policy." /><Connections kind="storage-locations" /></>; }
