import { useQuery } from "@tanstack/react-query";
import { Copy } from "lucide-react";
import { get } from "../../lib/api";
import type { Bootstrap } from "../../lib/types";
import { toast, useUI } from "../../lib/store";
import { AnvilMark, Button, Dialog, Wordmark } from "../../components/ui";

const REPO = "https://github.com/abdulwaheedsyed/Rowsmith";

/** A release's version, such as 0.1.0 or 0.1.0-rc.1. Builds from a checkout
 *  name the commit instead (0.1.0-4-g1a2b3c4, or just 1a2b3c4 before the first tag). */
export function isRelease(version: string) {
  return /^\d+\.\d+\.\d+(-[0-9A-Za-z.]+)?$/.test(version);
}

export function useVersion() {
  const boot = useQuery({ queryKey: ["bootstrap"], queryFn: () => get<Bootstrap>("bootstrap"), staleTime: Infinity });
  return boot.data?.version ?? "";
}

export function AboutDialog() {
  const { aboutOpen, setAboutOpen } = useUI();
  const version = useVersion();
  const release = isRelease(version);
  // Files as they were in this release; development builds link to main.
  const ref = release ? `v${version}` : "main";
  return (
    <Dialog open={aboutOpen} onOpenChange={setAboutOpen} title="About Rowsmith™">
      <div className="about">
        <AnvilMark size={56} />
        <Wordmark size={26} />
        <p className="about__tagline">A self-hosted web workspace for your databases</p>
        <div className="about__version">
          <span className="faint">{release ? "Version" : "Development build"}</span>
          <code className="mono">{version || "…"}</code>
          <Button size="sm" variant="ghost" icon aria-label="Copy version" disabled={!version} onClick={() => { navigator.clipboard?.writeText(version); toast.success("Version copied"); }}>
            <Copy />
          </Button>
        </div>
        <nav className="about__links" aria-label="Rowsmith links">
          {release && <a href={`${REPO}/releases/tag/v${version}`} target="_blank" rel="noreferrer">Release notes</a>}
          <a href={REPO} target="_blank" rel="noreferrer">Source code</a>
          <a href={`${REPO}/issues/new?template=bug_report.yml&version=${encodeURIComponent(version)}`} target="_blank" rel="noreferrer">Report a problem</a>
          <a href={`${REPO}/blob/${ref}/THIRD_PARTY_NOTICES.txt`} target="_blank" rel="noreferrer">Third-party notices</a>
        </nav>
        <p className="about__legal">
          Released under the <a href={`${REPO}/blob/${ref}/LICENSE`} target="_blank" rel="noreferrer">Apache License 2.0</a>.
          Rowsmith™ and the Rowsmith logo are trademarks of Syed Abdul Waheed.
        </p>
      </div>
    </Dialog>
  );
}
