import { ServiceTile } from "./cards";
import { buildVersionLabel, environmentLabel, formatDate, plural, relativeTime } from "../selectors";
import type { BuildInfo, EnvironmentGroup, Scan, Service, Tone, UptimeStat } from "../types";

type ServicesViewProps = {
  attentionCount: number;
  attentionOnly: boolean;
  buildVersion: BuildInfo | null;
  busyAction: string;
  error: string;
  filteredCount: number;
  groups: EnvironmentGroup[];
  lastChecked: string;
  latestScan: Scan | null;
  onAttentionOnlyChange: (attentionOnly: boolean) => void;
  onClearFilters: () => void;
  onOpenService: (serviceId: string) => void;
  onQueryChange: (query: string) => void;
  onRetry: () => void;
  onTrigger: (action: "scan" | "monitor") => void;
  overall: { tone: Tone; sentence: string };
  query: string;
  repositoryCount: number;
  services: Service[];
  uptimeByService: Map<string, UptimeStat[]>;
};

export function ServicesView({
  attentionCount,
  attentionOnly,
  buildVersion,
  busyAction,
  error,
  filteredCount,
  groups,
  lastChecked,
  latestScan,
  onAttentionOnlyChange,
  onClearFilters,
  onOpenService,
  onQueryChange,
  onRetry,
  onTrigger,
  overall,
  query,
  repositoryCount,
  services,
  uptimeByService
}: ServicesViewProps) {
  return (
    <div aria-labelledby="tab-services" className="tabPanel" id="servicesPanel" role="tabpanel">
      <section aria-live="polite" className="hero">
        <span aria-hidden="true" className={`beacon ${overall.tone}`} />
        <div>
          <p className="sentence">{overall.sentence}</p>
          <p className="heroMeta">
            {services.length} {plural(services.length, "service")}
            {" · "}
            {repositoryCount} {plural(repositoryCount, "repository", "repositories")}
            {lastChecked ? ` · checked ${relativeTime(lastChecked)}` : ""}
          </p>
        </div>
      </section>

      {error ? (
        <section className="errorBanner" role="alert">
          <span>Couldn&apos;t reach the dashboard: {error}</span>
          <button onClick={onRetry} type="button">Retry</button>
        </section>
      ) : null}

      <div className="toolbar">
        <label className="searchField">
          <span className="srOnly">Find a service</span>
          <input
            onChange={(event) => onQueryChange(event.target.value)}
            placeholder="Find a service"
            type="search"
            value={query}
          />
        </label>
        <button
          aria-pressed={attentionOnly}
          className={`filterPill ${attentionOnly ? "on" : ""}`}
          onClick={() => onAttentionOnlyChange(!attentionOnly)}
          type="button"
        >
          Needs attention{attentionCount > 0 ? ` (${attentionCount})` : ""}
        </button>
        <span className="toolbarGap" />
        <button
          className="action"
          disabled={busyAction !== ""}
          onClick={() => onTrigger("scan")}
          type="button"
        >
          {busyAction === "scan" ? "Syncing…" : "Sync repos"}
        </button>
        <button
          className="action primary"
          disabled={busyAction !== ""}
          onClick={() => onTrigger("monitor")}
          type="button"
        >
          {busyAction === "monitor" ? "Checking…" : "Check now"}
        </button>
      </div>

      <main>
        {groups.map((group) => (
          <section className="environment" key={group.environment}>
            <div className="environmentHead">
              <h2>{environmentLabel(group.environment)}</h2>
              <span className="tally">{group.upCount} of {group.services.length} up</span>
            </div>
            <div className="tiles">
              {group.services.map((service) => (
                <ServiceTile
                  key={service.id}
                  onOpen={() => onOpenService(service.id)}
                  service={service}
                  uptime={uptimeByService.get(service.id) ?? []}
                />
              ))}
            </div>
          </section>
        ))}
        {services.length === 0 ? (
          <div className="emptyState">
            <p className="emptyLead">Nothing here yet</p>
            <p>Sync repos to discover the services declared in Git.</p>
          </div>
        ) : null}
        {services.length > 0 && filteredCount === 0 ? (
          <div className="emptyState">
            <p className="emptyLead">No services match</p>
            <button className="action" onClick={onClearFilters} type="button">
              Clear filters
            </button>
          </div>
        ) : null}
      </main>

      <footer className="foot">
        <span>
          {latestScan
            ? <>Discovered from {repositoryCount} {plural(repositoryCount, "repository", "repositories")} · last sync <em className={latestScan.status === "ok" ? "ok" : "bad"}>{latestScan.status === "ok" ? "ok" : "failed"}</em>{latestScan.finishedAt ? ` · ${formatDate(latestScan.finishedAt)}` : ""}</>
            : "Not synced yet"}
        </span>
        {buildVersion ? <span>{buildVersionLabel(buildVersion)}</span> : null}
      </footer>
    </div>
  );
}
