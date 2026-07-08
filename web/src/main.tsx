import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource-variable/bricolage-grotesque/index.css";
import "@fontsource-variable/jetbrains-mono/index.css";
import "./styles.css";

type Health = "healthy" | "degraded" | "unhealthy" | "unknown" | "error" | "not_applicable";
type Theme = "light" | "dark";
type Tone = "steady" | "pending" | "watch" | "alert";

type Repository = {
  name: string;
  status: string;
  lastScanAt: string;
};

type Scan = {
  id: number;
  repository: string;
  status: string;
  finishedAt: string;
};

type Service = {
  id: string;
  name: string;
  repository: string;
  sourceCommit: string;
  sourcePath: string;
  runtime: string;
  kind: string;
  namespace: string;
  environment: string;
  health: Health;
  images: string[];
  dependencies: string[];
  exposure: string[];
};

type StatusResult = {
  serviceId: string;
  target: string;
  health: Health;
  message: string;
  checkedAt: string;
};

type UptimeSample = {
  health: Health;
  checkedAt: string;
  message: string;
};

type UptimeStat = {
  serviceId: string;
  target: string;
  uptimePercent: number;
  checkCount: number;
  samples: UptimeSample[];
};

type TargetSample = {
  target: string;
  sample: UptimeSample;
};

type MonitorTargetDetail = {
  target: string;
  status: StatusResult | null;
  uptime: UptimeStat | null;
};

type ContainerStatus = {
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  health: string;
  restartCount: number;
};

type AgentInfo = {
  target: string;
  lastSeenAt: string;
  configured: boolean;
  containers: ContainerStatus[];
};

type DashboardSummary = {
  repositories: Repository[];
  services: Service[];
  scans: Scan[];
  statuses: StatusResult[];
  uptime?: UptimeStat[];
  agents?: AgentInfo[];
  generatedAt: string;
};

type EnvironmentGroup = {
  environment: string;
  services: Service[];
  upCount: number;
};

type Tab = "services" | "agents";
type AgentConnection = "connected" | "stale" | "offline" | "never";

const statusWord: Record<Health, string> = {
  healthy: "Up",
  degraded: "Degraded",
  unhealthy: "Down",
  unknown: "No data",
  error: "Check failed",
  not_applicable: "Not applicable"
};

const tallyWord: Record<Health, string> = {
  healthy: "up",
  degraded: "degraded",
  unhealthy: "down",
  unknown: "unchecked",
  error: "failed",
  not_applicable: "not applicable"
};

const attentionStates: Health[] = ["degraded", "unhealthy", "error"];

const serviceSortOrder: Record<Health, number> = {
  error: 0,
  unhealthy: 1,
  degraded: 2,
  healthy: 3,
  unknown: 4,
  not_applicable: 5
};

const environmentSortOrder: Record<string, number> = {
  production: 0,
  staging: 1,
  homelab: 2,
  development: 3,
  testing: 4,
  local: 5,
  unassigned: 6
};

const agentConnectionWord: Record<AgentConnection, string> = {
  connected: "Connected",
  stale: "Stale",
  offline: "Offline",
  never: "Never connected"
};

const agentConnectionTone: Record<AgentConnection, Health> = {
  connected: "healthy",
  stale: "degraded",
  offline: "unhealthy",
  never: "unknown"
};

const agentConnectedThresholdMs = 120_000;
const agentStaleThresholdMs = 600_000;
const agentsTabHash = "#/agents";

const tileSlots = 28;
const drawerSlots = 40;
const refreshIntervalMs = 30_000;
const themeStorageKey = "gitops-dashboard-theme";

function App() {
  const [summary, setSummary] = useState<DashboardSummary | null>(null);
  const [error, setError] = useState<string>("");
  const [query, setQuery] = useState<string>("");
  const [attentionOnly, setAttentionOnly] = useState<boolean>(false);
  const [selectedServiceId, setSelectedServiceId] = useState<string>("");
  const [busyAction, setBusyAction] = useState<string>("");
  const [busyMonitorOverride, setBusyMonitorOverride] = useState<string>("");
  const [theme, setTheme] = useState<Theme>(initialTheme);
  const [activeTab, setActiveTab] = useState<Tab>(tabFromHash);
  const [selectedAgentTarget, setSelectedAgentTarget] = useState<string>("");
  const isInitialTabSync = useRef(true);

  const load = useCallback(async () => {
    try {
      const response = await fetch("/api/summary");
      if (!response.ok) {
        throw new Error(`summary request failed: ${response.status}`);
      }
      setSummary(await response.json() as DashboardSummary);
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load dashboard");
    }
  }, []);

  const trigger = useCallback(async (action: "scan" | "monitor") => {
    setBusyAction(action);
    try {
      const response = await fetch(`/api/${action}`, { method: "POST" });
      if (!response.ok) {
        throw new Error(`/api/${action} failed: ${response.status}`);
      }
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : `/api/${action} failed`);
    } finally {
      setBusyAction("");
    }
  }, [load]);

  const setMonitorNotApplicable = useCallback(async (serviceId: string, target: string, notApplicable: boolean) => {
    const key = monitorOverrideKey(serviceId, target);
    setBusyMonitorOverride(key);
    try {
      const response = await fetch("/api/monitor-overrides", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ serviceId, target, notApplicable })
      });
      if (!response.ok) {
        throw new Error(`/api/monitor-overrides failed: ${response.status}`);
      }
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : "monitor override failed");
    } finally {
      setBusyMonitorOverride("");
    }
  }, [load]);

  useEffect(() => {
    void load();
    const interval = window.setInterval(() => {
      if (document.visibilityState === "visible") {
        void load();
      }
    }, refreshIntervalMs);
    const onVisible = () => {
      if (document.visibilityState === "visible") {
        void load();
      }
    };
    document.addEventListener("visibilitychange", onVisible);
    return () => {
      window.clearInterval(interval);
      document.removeEventListener("visibilitychange", onVisible);
    };
  }, [load]);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem(themeStorageKey, theme);
  }, [theme]);

  useEffect(() => {
    document.body.classList.toggle("drawerOpen", selectedServiceId !== "" || selectedAgentTarget !== "");
  }, [selectedAgentTarget, selectedServiceId]);

  useEffect(() => {
    const onHashChange = () => setActiveTab(tabFromHash());
    window.addEventListener("hashchange", onHashChange);
    return () => window.removeEventListener("hashchange", onHashChange);
  }, []);

  useEffect(() => {
    if (isInitialTabSync.current) {
      isInitialTabSync.current = false;
      return;
    }
    const desiredHash = activeTab === "agents" ? agentsTabHash : "#/";
    if (window.location.hash !== desiredHash) {
      window.location.hash = desiredHash;
    }
  }, [activeTab]);

  const handleTabBarKeyDown = useCallback((event: React.KeyboardEvent<HTMLDivElement>) => {
    if (event.key !== "ArrowLeft" && event.key !== "ArrowRight") {
      return;
    }
    event.preventDefault();
    const nextTab: Tab = activeTab === "services" ? "agents" : "services";
    setActiveTab(nextTab);
    window.requestAnimationFrame(() => {
      document.getElementById(nextTab === "services" ? "tab-services" : "tab-agents")?.focus();
    });
  }, [activeTab]);

  const services = useMemo(() => summary?.services ?? [], [summary]);
  const uptime = useMemo(() => summary?.uptime ?? [], [summary]);
  const uptimeByService = useMemo(() => {
    const index = new Map<string, UptimeStat[]>();
    for (const stat of uptime) {
      index.set(stat.serviceId, [...(index.get(stat.serviceId) ?? []), stat]);
    }
    return index;
  }, [uptime]);

  const overall = useMemo(() => overallStatus(services), [services]);
  const lastChecked = useMemo(() => latestCheckTime(uptime, summary?.statuses ?? []), [summary, uptime]);

  const agents = useMemo(() => summary?.agents ?? [], [summary]);
  const agentOverall = useMemo(() => overallAgentStatus(agents), [agents]);
  const latestAgentReport = useMemo(() => latestAgentReportTime(agents), [agents]);
  const agentsNeedAttention = useMemo(
    () => agents.some((agent) => agentConnection(agent) !== "connected"),
    [agents]
  );

  const filtered = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return services.filter((service) => {
      if (attentionOnly && !attentionStates.includes(service.health)) {
        return false;
      }
      if (normalizedQuery === "") {
        return true;
      }
      return searchableServiceText(service).includes(normalizedQuery);
    });
  }, [attentionOnly, query, services]);

  const groups = useMemo(() => groupByEnvironment(filtered), [filtered]);
  const attentionCount = useMemo(
    () => services.filter((service) => attentionStates.includes(service.health)).length,
    [services]
  );

  const selectedService = services.find((service) => service.id === selectedServiceId) ?? null;
  const selectedAgent = agents.find((agent) => agent.target === selectedAgentTarget) ?? null;
  const latestScan = summary?.scans[0] ?? null;
  const repositoryCount = summary?.repositories.length ?? 0;

  return (
    <div className="shell">
      <header className="masthead">
        <div className="wordmark">
          <span className="mark" aria-hidden="true"><i /><i /><i /><i /><i /></span>
          <h1>GitOps Dashboard</h1>
        </div>
        <button
          aria-label="Use dark theme"
          aria-pressed={theme === "dark"}
          className="themeButton"
          onClick={() => setTheme(theme === "dark" ? "light" : "dark")}
          type="button"
        >
          <span aria-hidden="true">{theme === "dark" ? "☾" : "☀"}</span>
        </button>
      </header>

      <div aria-label="Dashboard views" className="tabBar" onKeyDown={handleTabBarKeyDown} role="tablist">
        <button
          aria-controls="servicesPanel"
          aria-selected={activeTab === "services"}
          className={`tab ${activeTab === "services" ? "active" : ""}`}
          id="tab-services"
          onClick={() => setActiveTab("services")}
          role="tab"
          tabIndex={activeTab === "services" ? 0 : -1}
          type="button"
        >
          Services
        </button>
        <button
          aria-controls="agentsPanel"
          aria-selected={activeTab === "agents"}
          className={`tab ${activeTab === "agents" ? "active" : ""}`}
          id="tab-agents"
          onClick={() => setActiveTab("agents")}
          role="tab"
          tabIndex={activeTab === "agents" ? 0 : -1}
          type="button"
        >
          Agents
          {agentsNeedAttention ? <span aria-hidden="true" className="tabDot" /> : null}
        </button>
      </div>

      {activeTab === "services" ? (
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
              <button onClick={() => void load()} type="button">Retry</button>
            </section>
          ) : null}

          <div className="toolbar">
            <label className="searchField">
              <span className="srOnly">Find a service</span>
              <input
                onChange={(event) => setQuery(event.target.value)}
                placeholder="Find a service"
                type="search"
                value={query}
              />
            </label>
            <button
              aria-pressed={attentionOnly}
              className={`filterPill ${attentionOnly ? "on" : ""}`}
              onClick={() => setAttentionOnly(!attentionOnly)}
              type="button"
            >
              Needs attention{attentionCount > 0 ? ` (${attentionCount})` : ""}
            </button>
            <span className="toolbarGap" />
            <button
              className="action"
              disabled={busyAction !== ""}
              onClick={() => void trigger("scan")}
              type="button"
            >
              {busyAction === "scan" ? "Syncing…" : "Sync repos"}
            </button>
            <button
              className="action primary"
              disabled={busyAction !== ""}
              onClick={() => void trigger("monitor")}
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
                      onOpen={() => setSelectedServiceId(service.id)}
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
            {services.length > 0 && filtered.length === 0 ? (
              <div className="emptyState">
                <p className="emptyLead">No services match</p>
                <button
                  className="action"
                  onClick={() => {
                    setQuery("");
                    setAttentionOnly(false);
                  }}
                  type="button"
                >
                  Clear filters
                </button>
              </div>
            ) : null}
          </main>

          <footer className="foot">
            {latestScan
              ? <span>Discovered from {repositoryCount} {plural(repositoryCount, "repository", "repositories")} · last sync <em className={latestScan.status === "ok" ? "ok" : "bad"}>{latestScan.status === "ok" ? "ok" : "failed"}</em>{latestScan.finishedAt ? ` · ${formatDate(latestScan.finishedAt)}` : ""}</span>
              : <span>Not synced yet</span>}
          </footer>
        </div>
      ) : (
        <div aria-labelledby="tab-agents" className="tabPanel" id="agentsPanel" role="tabpanel">
          <section aria-live="polite" className="hero">
            <span aria-hidden="true" className={`beacon ${agentOverall.tone}`} />
            <div>
              <p className="sentence">{agentOverall.sentence}</p>
              <p className="heroMeta">
                {agents.length} {plural(agents.length, "agent")}
                {latestAgentReport ? ` · last report ${relativeTime(latestAgentReport)}` : ""}
              </p>
            </div>
          </section>

          <main>
            {agents.length === 0 ? (
              <div className="emptyState">
                <p className="emptyLead">No agents yet</p>
                <p>Agents connect once you run this binary with mode: agent pointed at this server.</p>
              </div>
            ) : (
              <div className="tiles">
                {agents.map((agent) => (
                  <AgentCard agent={agent} key={agent.target} onOpen={() => setSelectedAgentTarget(agent.target)} />
                ))}
              </div>
            )}
          </main>
        </div>
      )}

      {selectedService ? (
        <ServiceDrawer
          busyMonitorOverride={busyMonitorOverride}
          onClose={() => setSelectedServiceId("")}
          onSetMonitorNotApplicable={setMonitorNotApplicable}
          service={selectedService}
          statuses={(summary?.statuses ?? []).filter((status) => status.serviceId === selectedService.id)}
          uptime={uptimeByService.get(selectedService.id) ?? []}
        />
      ) : null}

      {selectedAgent ? (
        <AgentDrawer agent={selectedAgent} onClose={() => setSelectedAgentTarget("")} />
      ) : null}
    </div>
  );
}

function ServiceTile({ onOpen, service, uptime }: {
  onOpen: () => void;
  service: Service;
  uptime: UptimeStat[];
}) {
  const routes = accessTargets(service);
  const door = routes[0] ?? null;
  const samples = aggregateUptimeSamples(uptime);
  const percent = worstPercent(uptime);
  const lastSample = samples[samples.length - 1] ?? null;

  return (
    <article
      aria-label={`${service.name}, ${statusWord[service.health]}`}
      className={`tile ${service.health}`}
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onOpen();
        }
      }}
      role="button"
      tabIndex={0}
    >
      <div className="tileTop">
        <span aria-hidden="true" className={`dot ${service.health}`} />
        <h3>{service.name}</h3>
        <span className={`stateWord ${service.health}`}>{statusWord[service.health]}</span>
      </div>
      {door ? (
        <span className="doorRow">
          <a
            className="door"
            href={door.href}
            onClick={(event) => event.stopPropagation()}
            rel="noreferrer"
            target="_blank"
          >
            {door.label}<span aria-hidden="true" className="doorArrow">{"↗"}</span>
          </a>
          {routes.length > 1 ? <span className="doorMore">+{routes.length - 1}</span> : null}
        </span>
      ) : (
        <span className="doorRow doorNone">no route in Git</span>
      )}
      <PulseStrip samples={samples} slots={tileSlots} />
      <div className="tileFoot">
        <span>{percent === null ? "no checks yet" : `${percent}% · 24h`}</span>
        <span>{lastSample ? relativeTime(lastSample.checkedAt) : ""}</span>
      </div>
    </article>
  );
}

function PulseStrip({ samples, slots, wide }: { samples: UptimeSample[]; slots: number; wide?: boolean }) {
  const recent = samples.slice(-slots);
  const emptyCount = Math.max(0, slots - recent.length);
  return (
    <div aria-label={stripLabel(recent)} className={`pulseStrip ${wide ? "wide" : ""}`} role="img">
      {Array.from({ length: emptyCount }, (_, index) => (
        <span aria-hidden="true" className="tick empty" key={`empty-${index}`} />
      ))}
      {recent.map((sample, index) => (
        <span
          aria-hidden="true"
          className={`tick ${sample.health}`}
          key={`${sample.checkedAt}-${index}`}
          title={`${statusWord[sample.health]} · ${formatDate(sample.checkedAt)}${sample.message ? ` · ${sample.message}` : ""}`}
        />
      ))}
    </div>
  );
}

function ServiceDrawer({ busyMonitorOverride, onClose, onSetMonitorNotApplicable, service, statuses, uptime }: {
  busyMonitorOverride: string;
  onClose: () => void;
  onSetMonitorNotApplicable: (serviceId: string, target: string, notApplicable: boolean) => void;
  service: Service;
  statuses: StatusResult[];
  uptime: UptimeStat[];
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const routes = accessTargets(service);
  const commit = service.sourceCommit ? service.sourceCommit.slice(0, 7) : "";
  const targets = monitorTargetDetails(statuses, uptime);

  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <>
      <div aria-hidden="true" className="scrim" onClick={onClose} />
      <aside aria-labelledby="drawerTitle" aria-modal="true" className="drawer" role="dialog">
        <header className="drawerHead">
          <span aria-hidden="true" className={`dot big ${service.health}`} />
          <div className="drawerTitleBlock">
            <h2 id="drawerTitle">{service.name}</h2>
            <p className="drawerSub">
              {statusWord[service.health]} {"·"} {runtimeLabel(service.runtime)} {"·"} {environmentLabel(service.environment || "unassigned")}
            </p>
          </div>
          <button aria-label="Close details" className="drawerClose" onClick={onClose} ref={closeRef} type="button">
            <span aria-hidden="true">{"✕"}</span>
          </button>
        </header>

        <section className="drawerSection">
          <h3>Open</h3>
          {routes.length ? (
            <ul className="routeList">
              {routes.map((route) => (
                <li key={route.href}>
                  <a href={route.href} rel="noreferrer" target="_blank">
                    <span className="routeHost">{route.label}</span>
                    <span aria-hidden="true" className="doorArrow">{"↗"}</span>
                  </a>
                </li>
              ))}
            </ul>
          ) : (
            <p className="quiet">
              {service.runtime === "host" ? "No openable route was found for this host." : "No routes or DNS names were found in Git for this service."}
            </p>
          )}
        </section>

        <section className="drawerSection">
          <h3>Uptime</h3>
          {targets.length ? targets.map((target) => {
            const last = target.uptime?.samples[target.uptime.samples.length - 1] ?? null;
            const ignored = target.status?.health === "not_applicable";
            const busy = busyMonitorOverride === monitorOverrideKey(service.id, target.target);
            return (
              <div className={`targetBlock ${ignored ? "notApplicable" : ""}`} key={target.target}>
                <div className="targetHead">
                  <strong>{target.target}</strong>
                  <span>{targetDetailMeta(target)}</span>
                </div>
                {target.uptime ? <PulseStrip samples={target.uptime.samples} slots={drawerSlots} wide /> : null}
                {ignored ? (
                  <p className="targetNote">
                    {statusWord.not_applicable}{target.status?.message ? ` — ${target.status.message}` : ""}
                  </p>
                ) : last ? (
                  <p className="targetNote">
                    {statusWord[last.health]}{last.message ? ` — ${last.message}` : ""} {"·"} {relativeTime(last.checkedAt)}
                  </p>
                ) : target.status ? (
                  <p className="targetNote">
                    {statusWord[target.status.health]}{target.status.message ? ` — ${target.status.message}` : ""} {"·"} {relativeTime(target.status.checkedAt)}
                  </p>
                ) : null}
                <button
                  className="targetToggle"
                  disabled={busy}
                  onClick={() => onSetMonitorNotApplicable(service.id, target.target, !ignored)}
                  type="button"
                >
                  {busy ? "Saving..." : ignored ? "Enable monitor" : "Mark not applicable"}
                </button>
              </div>
            );
          }) : (
            <p className="quiet">No checks yet. Check now to see live status.</p>
          )}
        </section>

        {service.dependencies.length ? (
          <section className="drawerSection">
            <h3>Depends on</h3>
            <div className="chips">
              {service.dependencies.map((dependency) => (
                <span className="chip" key={dependency}>{dependency}</span>
              ))}
            </div>
          </section>
        ) : null}

        <section className="drawerSection">
          <h3>{service.runtime === "host" ? "Configured from" : "Declared in Git"}</h3>
          <p className="provenance">
            {service.repository} {"·"} {service.sourcePath}{commit ? ` @ ${commit}` : ""}
          </p>
          {service.images.length ? <p className="provenance quiet">{service.images.join(", ")}</p> : null}
        </section>
      </aside>
    </>
  );
}

function AgentCard({ agent, onOpen }: { agent: AgentInfo; onOpen: () => void }) {
  const connection = agentConnection(agent);
  const tone = agentCardTone(connection, agent.containers);
  const wordTone = agentConnectionTone[connection];

  return (
    <article
      aria-label={`${agent.target}, ${agentConnectionWord[connection]}`}
      className={`tile ${tone}`}
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onOpen();
        }
      }}
      role="button"
      tabIndex={0}
    >
      <div className="tileTop">
        <span aria-hidden="true" className={`dot ${wordTone}`} />
        <h3>{agent.target}</h3>
        <span className={`stateWord ${wordTone}`}>{agentConnectionWord[connection]}</span>
      </div>
      <div className="tileFoot">
        <span>{agent.lastSeenAt ? `last report ${relativeTime(agent.lastSeenAt)}` : "no reports yet"}</span>
        <span>{containerTally(agent.containers)}</span>
      </div>
    </article>
  );
}

function AgentDrawer({ agent, onClose }: { agent: AgentInfo; onClose: () => void }) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const connection = agentConnection(agent);
  const wordTone = agentConnectionTone[connection];
  const containers = useMemo(() => sortContainers(agent.containers), [agent.containers]);

  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <>
      <div aria-hidden="true" className="scrim" onClick={onClose} />
      <aside aria-labelledby="agentDrawerTitle" aria-modal="true" className="drawer" role="dialog">
        <header className="drawerHead">
          <span aria-hidden="true" className={`dot big ${wordTone}`} />
          <div className="drawerTitleBlock">
            <h2 id="agentDrawerTitle">{agent.target}</h2>
            <p className="drawerSub">
              {agentConnectionWord[connection]} {"·"} {agent.lastSeenAt ? `last report ${relativeTime(agent.lastSeenAt)}` : "no reports yet"}
            </p>
          </div>
          <button aria-label="Close details" className="drawerClose" onClick={onClose} ref={closeRef} type="button">
            <span aria-hidden="true">{"✕"}</span>
          </button>
        </header>

        <section className="drawerSection">
          <h3>Containers</h3>
          {containers.length ? (
            <ul className="containerList">
              {containers.map((container) => (
                <li key={container.id || container.name}>
                  <div className="containerRow">
                    <div className="containerName">
                      <strong>{container.name}</strong>
                      <span className="containerImage">{container.image}</span>
                    </div>
                    <span className={`stateWord ${containerTone(container)}`}>{containerWord(container)}</span>
                  </div>
                  {container.restartCount > 0 ? (
                    <p className="containerRestarts">{container.restartCount} {plural(container.restartCount, "restart")}</p>
                  ) : null}
                </li>
              ))}
            </ul>
          ) : (
            <p className="quiet">No containers reported.</p>
          )}
        </section>
      </aside>
    </>
  );
}

function tabFromHash(): Tab {
  return window.location.hash === agentsTabHash ? "agents" : "services";
}

function agentConnection(agent: AgentInfo): AgentConnection {
  if (!agent.lastSeenAt) {
    return "never";
  }
  const seenAt = new Date(agent.lastSeenAt).getTime();
  if (Number.isNaN(seenAt)) {
    return "never";
  }
  const elapsedMs = Date.now() - seenAt;
  if (elapsedMs < agentConnectedThresholdMs) {
    return "connected";
  }
  if (elapsedMs < agentStaleThresholdMs) {
    return "stale";
  }
  return "offline";
}

function isContainerRunning(container: ContainerStatus): boolean {
  return container.state === "running" && container.health !== "unhealthy";
}

function agentCardTone(connection: AgentConnection, containers: ContainerStatus[]): Health {
  if (connection === "connected") {
    return containers.some((container) => !isContainerRunning(container)) ? "degraded" : "healthy";
  }
  return agentConnectionTone[connection];
}

function overallAgentStatus(agents: AgentInfo[]): { tone: Tone; sentence: string } {
  if (agents.length === 0) {
    return { tone: "pending", sentence: "No agents connected yet" };
  }
  const connections = agents.map(agentConnection);
  const attention = connections.filter((connection) => connection !== "connected").length;
  if (attention === 0) {
    return { tone: "steady", sentence: `All ${agents.length} ${plural(agents.length, "agent")} connected` };
  }
  const tone: Tone = connections.some((connection) => connection === "offline" || connection === "never")
    ? "alert"
    : "watch";
  return { tone, sentence: `${attention} ${plural(attention, "agent needs", "agents need")} attention` };
}

function latestAgentReportTime(agents: AgentInfo[]): string {
  let latest = "";
  for (const agent of agents) {
    if (agent.lastSeenAt && agent.lastSeenAt > latest) {
      latest = agent.lastSeenAt;
    }
  }
  return latest;
}

function containerTally(containers: ContainerStatus[]): string {
  if (containers.length === 0) {
    return "no containers reported";
  }
  const running = containers.filter(isContainerRunning).length;
  return `${running} of ${containers.length} running`;
}

function containerTone(container: ContainerStatus): Health {
  if (!isContainerRunning(container)) {
    return "unhealthy";
  }
  return container.health === "starting" ? "degraded" : "healthy";
}

function containerWord(container: ContainerStatus): string {
  if (container.state !== "running") {
    return titleize(container.state || "unknown");
  }
  if (container.health === "unhealthy") {
    return "Unhealthy";
  }
  if (container.health === "starting") {
    return "Starting";
  }
  return "Running";
}

function sortContainers(containers: ContainerStatus[]): ContainerStatus[] {
  return [...containers].sort((left, right) => {
    const leftRunning = isContainerRunning(left) ? 1 : 0;
    const rightRunning = isContainerRunning(right) ? 1 : 0;
    return leftRunning - rightRunning || left.name.localeCompare(right.name);
  });
}

function overallStatus(services: Service[]): { tone: Tone; sentence: string } {
  if (services.length === 0) {
    return { tone: "pending", sentence: "Waiting for the first scan" };
  }
  const counts = countByHealth(services);
  const attention = counts.degraded + counts.unhealthy + counts.error;
  if (attention > 0) {
    const tone: Tone = counts.unhealthy + counts.error > 0 ? "alert" : "watch";
    return { tone, sentence: `${attention} ${plural(attention, "service needs", "services need")} attention` };
  }
  if (counts.healthy === 0) {
    return { tone: "pending", sentence: "Waiting for live checks" };
  }
  if (counts.unknown > 0) {
    return { tone: "steady", sentence: "Everything checked is up" };
  }
  return { tone: "steady", sentence: "All systems up" };
}

function countByHealth(services: Service[]) {
  const counts: Record<Health, number> = {
    healthy: 0,
    degraded: 0,
    unhealthy: 0,
    unknown: 0,
    error: 0,
    not_applicable: 0
  };
  for (const service of services) {
    counts[service.health] += 1;
  }
  return counts;
}

function groupByEnvironment(services: Service[]): EnvironmentGroup[] {
  const byEnvironment = new Map<string, Service[]>();
  for (const service of services) {
    const environment = service.environment || "unassigned";
    byEnvironment.set(environment, [...(byEnvironment.get(environment) ?? []), service]);
  }
  return Array.from(byEnvironment.entries())
    .map(([environment, environmentServices]) => {
      const sorted = [...environmentServices].sort((left, right) => {
        return serviceSortOrder[left.health] - serviceSortOrder[right.health] ||
          left.name.localeCompare(right.name);
      });
      return {
        environment,
        services: sorted,
        upCount: sorted.filter((service) => service.health === "healthy").length
      };
    })
    .sort((left, right) => {
      const leftOrder = environmentSortOrder[left.environment] ?? 99;
      const rightOrder = environmentSortOrder[right.environment] ?? 99;
      return leftOrder - rightOrder || left.environment.localeCompare(right.environment);
    });
}

function aggregateUptimeSamples(stats: UptimeStat[]): UptimeSample[] {
  const measured = stats.filter((stat) => stat.samples.length > 0);
  if (measured.length === 0) {
    return [];
  }
  if (measured.length === 1) {
    return measured[0].samples;
  }
  const sampleCount = Math.max(...measured.map((stat) => stat.samples.length));
  const samples: UptimeSample[] = [];
  for (let offsetFromEnd = sampleCount - 1; offsetFromEnd >= 0; offsetFromEnd -= 1) {
    const targetSamples: TargetSample[] = [];
    for (const stat of measured) {
      const index = stat.samples.length - 1 - offsetFromEnd;
      if (index >= 0) {
        targetSamples.push({ target: stat.target, sample: stat.samples[index] });
      }
    }
    if (targetSamples.length > 0) {
      samples.push({
        health: aggregateSampleHealth(targetSamples.map(({ sample }) => sample.health)),
        checkedAt: latestSampleTime(targetSamples),
        message: aggregateSampleMessage(targetSamples)
      });
    }
  }
  return samples;
}

function aggregateSampleHealth(healths: Health[]): Health {
  healths = healths.filter((health) => health !== "not_applicable");
  if (healths.length === 0) {
    return "unknown";
  }
  if (healths.length === 1) {
    return healths[0];
  }
  const allHealthy = healths.every((health) => health === "healthy");
  if (allHealthy) {
    return "healthy";
  }
  const anyHealthy = healths.some((health) => health === "healthy");
  if (anyHealthy) {
    return "degraded";
  }
  const allUnknown = healths.every((health) => health === "unknown");
  if (allUnknown) {
    return "unknown";
  }
  return healths.reduce((worst, health) => (
    serviceSortOrder[health] < serviceSortOrder[worst] ? health : worst
  ), "unknown");
}

function latestSampleTime(samples: TargetSample[]): string {
  return samples.reduce((latest, { sample }) => (
    sample.checkedAt > latest ? sample.checkedAt : latest
  ), "");
}

function aggregateSampleMessage(samples: TargetSample[]): string {
  samples = samples.filter(({ sample }) => sample.health !== "not_applicable");
  const failing = samples.filter(({ sample }) => sample.health !== "healthy");
  if (failing.length === 0) {
    return samples.length > 1 ? "All checks passed" : samples[0]?.sample.message ?? "";
  }
  if (samples.length === 1) {
    return failing[0]?.sample.message ?? "";
  }
  return failing
    .map(({ target, sample }) => `${target}: ${sample.message || statusWord[sample.health]}`)
    .join("; ");
}

function worstPercent(stats: UptimeStat[]): number | null {
  const measured = stats.filter((stat) => stat.checkCount > 0);
  if (measured.length === 0) {
    return null;
  }
  return measured.reduce((worst, stat) => Math.min(worst, stat.uptimePercent), 100);
}

function latestCheckTime(uptime: UptimeStat[], statuses: StatusResult[]): string {
  let latest = "";
  for (const stat of uptime) {
    const last = stat.samples[stat.samples.length - 1];
    if (last && last.checkedAt > latest) {
      latest = last.checkedAt;
    }
  }
  for (const status of statuses) {
    if (status.health === "not_applicable") {
      continue;
    }
    if (status.checkedAt && status.checkedAt > latest) {
      latest = status.checkedAt;
    }
  }
  return latest;
}

function monitorTargetDetails(statuses: StatusResult[], uptime: UptimeStat[]): MonitorTargetDetail[] {
  const byTarget = new Map<string, MonitorTargetDetail>();
  for (const stat of uptime) {
    byTarget.set(stat.target, { target: stat.target, status: null, uptime: stat });
  }
  for (const status of statuses) {
    const detail = byTarget.get(status.target);
    if (detail) {
      detail.status = status;
    } else {
      byTarget.set(status.target, { target: status.target, status, uptime: null });
    }
  }
  return Array.from(byTarget.values()).sort((left, right) => left.target.localeCompare(right.target));
}

function targetDetailMeta(target: MonitorTargetDetail): string {
  if (target.status?.health === "not_applicable") {
    return "not applicable";
  }
  if (target.uptime && target.uptime.checkCount > 0) {
    return `${target.uptime.uptimePercent}% · ${target.uptime.checkCount} ${plural(target.uptime.checkCount, "check")} · 24h`;
  }
  if (target.status) {
    return statusWord[target.status.health];
  }
  return "no checks yet";
}

function monitorOverrideKey(serviceId: string, target: string): string {
  return `${serviceId}\u0000${target}`;
}

function stripLabel(samples: UptimeSample[]): string {
  if (samples.length === 0) {
    return "No checks recorded yet";
  }
  const counts = new Map<Health, number>();
  for (const sample of samples) {
    counts.set(sample.health, (counts.get(sample.health) ?? 0) + 1);
  }
  const parts = Array.from(counts.entries()).map(([health, count]) => `${count} ${tallyWord[health]}`);
  return `Last ${samples.length} ${plural(samples.length, "check")}: ${parts.join(", ")}`;
}

function initialTheme(): Theme {
  const savedTheme = window.localStorage.getItem(themeStorageKey);
  if (savedTheme === "light" || savedTheme === "dark") {
    return savedTheme;
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

function searchableServiceText(service: Service) {
  return [
    service.name,
    service.repository,
    service.runtime,
    service.environment,
    service.kind,
    ...service.exposure
  ].join(" ").toLowerCase();
}

function accessTargets(service: Service) {
  const targets = new Map<string, { href: string; label: string }>();
  for (const route of service.exposure.filter(isAccessRoute)) {
    const href = hrefForRoute(route);
    const key = href.replace(/\/$/, "");
    if (!targets.has(key)) {
      targets.set(key, { href, label: labelForRoute(route) });
    }
  }
  return Array.from(targets.values());
}

function isAccessRoute(value: string) {
  const host = hostForRoute(value);
  if (isClusterInternalHost(host)) {
    return false;
  }
  if (/^(https?|ssh):\/\//.test(value)) {
    return true;
  }
  if (/^\d{1,3}(\.\d{1,3}){3}(:\d+)?(\/.*)?$/.test(value)) {
    return true;
  }
  return /^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:\d+)?(\/.*)?$/i.test(value);
}

function hrefForRoute(value: string) {
  if (/^(https?|ssh):\/\//.test(value)) {
    return value;
  }
  const host = hostForRoute(value);
  const scheme = host.endsWith(".lan") || /^\d{1,3}(\.\d{1,3}){3}$/.test(host) ? "http" : "https";
  return `${scheme}://${value}`;
}

function labelForRoute(value: string) {
  return value.replace(/^(https?|ssh):\/\//, "").replace(/\/$/, "");
}

function hostForRoute(value: string) {
  if (/^(https?|ssh):\/\//.test(value)) {
    try {
      return new URL(value).hostname;
    } catch {
      return "";
    }
  }
  return value.split(/[/:]/, 1)[0] ?? "";
}

function isClusterInternalHost(host: string) {
  return host.endsWith(".svc") || host.includes(".svc.") || host.endsWith(".cluster.local");
}

function environmentLabel(value: string) {
  return titleize(value || "unassigned");
}

function runtimeLabel(value: string) {
  if (value === "kubernetes") {
    return "Kubernetes";
  }
  if (value === "host") {
    return "Host";
  }
  return titleize(value || "other");
}

function titleize(value: string) {
  return value
    .split(/[-_\s]+/)
    .filter(Boolean)
    .map((part) => `${part.charAt(0).toUpperCase()}${part.slice(1)}`)
    .join(" ") || "Unassigned";
}

function plural(count: number, singular: string, pluralForm?: string) {
  return count === 1 ? singular : pluralForm ?? `${singular}s`;
}

function relativeTime(value: string) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return "";
  }
  const seconds = Math.max(0, Math.round((Date.now() - date.getTime()) / 1000));
  if (seconds < 45) {
    return "just now";
  }
  if (seconds < 90) {
    return "1 min ago";
  }
  if (seconds < 3600) {
    return `${Math.round(seconds / 60)} min ago`;
  }
  if (seconds < 86_400) {
    return `${Math.round(seconds / 3600)} h ago`;
  }
  return formatDate(value);
}

function formatDate(value: string) {
  if (!value) {
    return "";
  }
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) {
    return value;
  }
  return date.toLocaleString(undefined, {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit"
  });
}

createRoot(document.getElementById("root")!).render(<App />);
