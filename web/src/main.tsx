import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource-variable/bricolage-grotesque/index.css";
import "@fontsource-variable/jetbrains-mono/index.css";
import "./styles.css";

type Health = "healthy" | "degraded" | "unhealthy" | "unknown" | "error";
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

type DashboardSummary = {
  repositories: Repository[];
  services: Service[];
  scans: Scan[];
  statuses: StatusResult[];
  uptime?: UptimeStat[];
  generatedAt: string;
};

type EnvironmentGroup = {
  environment: string;
  services: Service[];
  upCount: number;
};

const statusWord: Record<Health, string> = {
  healthy: "Up",
  degraded: "Degraded",
  unhealthy: "Down",
  unknown: "No data",
  error: "Check failed"
};

const tallyWord: Record<Health, string> = {
  healthy: "up",
  degraded: "degraded",
  unhealthy: "down",
  unknown: "unchecked",
  error: "failed"
};

const attentionStates: Health[] = ["degraded", "unhealthy", "error"];

const serviceSortOrder: Record<Health, number> = {
  error: 0,
  unhealthy: 1,
  degraded: 2,
  healthy: 3,
  unknown: 4
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
  const [theme, setTheme] = useState<Theme>(initialTheme);

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
    document.body.classList.toggle("drawerOpen", selectedServiceId !== "");
  }, [selectedServiceId]);

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

      {selectedService ? (
        <ServiceDrawer
          onClose={() => setSelectedServiceId("")}
          service={selectedService}
          statuses={(summary?.statuses ?? []).filter((status) => status.serviceId === selectedService.id)}
          uptime={uptimeByService.get(selectedService.id) ?? []}
        />
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
  const primary = primaryUptime(uptime);
  const percent = worstPercent(uptime);
  const lastSample = primary?.samples[primary.samples.length - 1] ?? null;

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
      <PulseStrip samples={primary?.samples ?? []} slots={tileSlots} />
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

function ServiceDrawer({ onClose, service, statuses, uptime }: {
  onClose: () => void;
  service: Service;
  statuses: StatusResult[];
  uptime: UptimeStat[];
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const routes = accessTargets(service);
  const commit = service.sourceCommit ? service.sourceCommit.slice(0, 7) : "";

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
            <p className="quiet">No routes or DNS names were found in Git for this service.</p>
          )}
        </section>

        <section className="drawerSection">
          <h3>Uptime</h3>
          {uptime.length ? uptime.map((stat) => {
            const last = stat.samples[stat.samples.length - 1] ?? null;
            return (
              <div className="targetBlock" key={stat.target}>
                <div className="targetHead">
                  <strong>{stat.target}</strong>
                  <span>{stat.checkCount > 0 ? `${stat.uptimePercent}% · ${stat.checkCount} ${plural(stat.checkCount, "check")} · 24h` : "no checks yet"}</span>
                </div>
                <PulseStrip samples={stat.samples} slots={drawerSlots} wide />
                {last ? (
                  <p className="targetNote">
                    {statusWord[last.health]}{last.message ? ` — ${last.message}` : ""} {"·"} {relativeTime(last.checkedAt)}
                  </p>
                ) : null}
              </div>
            );
          }) : statuses.length ? (
            <ul className="statusFallback">
              {statuses.map((status) => (
                <li key={`${status.serviceId}-${status.target}`}>
                  <strong>{status.target}</strong> {statusWord[status.health]}{status.message ? ` — ${status.message}` : ""}
                </li>
              ))}
            </ul>
          ) : (
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
          <h3>Declared in Git</h3>
          <p className="provenance">
            {service.repository} {"·"} {service.sourcePath}{commit ? ` @ ${commit}` : ""}
          </p>
          {service.images.length ? <p className="provenance quiet">{service.images.join(", ")}</p> : null}
        </section>
      </aside>
    </>
  );
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
    error: 0
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

function primaryUptime(stats: UptimeStat[]): UptimeStat | null {
  let primary: UptimeStat | null = null;
  let primaryTime = "";
  for (const stat of stats) {
    const last = stat.samples[stat.samples.length - 1];
    const time = last ? last.checkedAt : "";
    if (!primary || time > primaryTime) {
      primary = stat;
      primaryTime = time;
    }
  }
  return primary;
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
    if (status.checkedAt && status.checkedAt > latest) {
      latest = status.checkedAt;
    }
  }
  return latest;
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
