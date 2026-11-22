import { Plug } from "lucide-react";
import { GitHubAppSetup } from "../components/GitHubApp";
import { CloudflareSettings } from "../components/Cloudflare";
import { GitConnections } from "../components/GitConnections";
import { PageHeader, Section, SectionGrid } from "../components/ui";

export function IntegrationsPage() {
  return <>
    <PageHeader icon={<Plug aria-hidden />} title="Integrations" description="Connect external services to Cove. Connections belong to you; share what each team can use." />
    <SectionGrid>
      <Section title="GitHub App" description="One app connects personal and organization repositories. Connect accounts to your user, then share them with teams." flush><GitHubAppSetup /></Section>
      <Section title="Git providers" description="Connect GitLab, Gitea, Forgejo, Codeberg or Bitbucket Cloud with an access token. Connections belong to the current team." flush><GitConnections /></Section>
      <Section title="Cloudflare DNS" description="Keep DNS records for shared domains pointed at the workers that serve them." flush><CloudflareSettings /></Section>
    </SectionGrid>
  </>;
}
