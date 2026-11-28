import { Container } from "lucide-react";
import { Connections } from "../components/Connections";
import { ManagedRegistries } from "../components/ManagedRegistries";
import { PageHeader } from "../components/ui";
export function RegistriesPage() { return <><PageHeader icon={<Container aria-hidden />} title="Docker registries" description="Team registry credentials for private images. Select a registry in each resource’s settings." /><Connections kind="registries" /><ManagedRegistries /></>; }
