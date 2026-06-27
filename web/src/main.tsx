import React, { useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

type Health = "healthy" | "degraded" | "unhealthy" | "unknown" | "error";
type RuntimeFilter = "all" | "compose" | "kubernetes";
type HealthFilter = "all" | Health;
type Theme = "light" | "dark";

type Repository = {
  name: string;
};

type Scan = {
  id: number;
  status: string;
};

type Service = {
  id: string;
  name: string;
  repository: string;
  runtime: string;
  environment: string;
  health: Health;
  exposure: string[];
};

type StatusResult = {
  serviceId: string;
  target: string;
  health: Health;
  message: string;
  checkedAt: string;
};

type DashboardSummary = {
  repositories: Repository[];
  services: Service[];
  scans: Scan[];
  statuses: StatusResult[];
  generatedAt: string;
};

const healthOrder: Health[] = ["healthy", "degraded", "unhealthy", "unknown", "error"];

const stateLabel: Record<Health, string> = {
  healthy: "Healthy",
  degraded: "Degraded",
  unhealthy: "Unhealthy",
  unknown: "Unknown",
  error: "Error"
};

const runtimeLabels: Record<RuntimeFilter, string> = {
  all: "All",
  compose: "Compose",
  kubernetes: "Kubernetes"
};

const themeStorageKey = "gitops-dashboard-theme";

function App() {
  const [summary, setSummary] = useState<DashboardSummary | null>(null);
  const [error, setError] = useState<string>("");
  const [selectedRuntime, setSelectedRuntime] = useState<RuntimeFilter>("all");
  const [selectedHealth, setSelectedHealth] = useState<HealthFilter>("all");
  const [selectedServiceId, setSelectedServiceId] = useState<string>("");
  const [query, setQuery] = useState<string>("");
  const [theme, setTheme] = useState<Theme>(initialTheme);

  async function load() {
    setError("");
    try {
      const response = await fetch("/api/summary");
      if (!response.ok) {
        throw new Error(`summary request failed: ${response.status}`);
      }
      setSummary(await response.json() as DashboardSummary);
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load dashboard");
    }
  }

  async function post(path: string) {
    setError("");
    try {
      const response = await fetch(path, { method: "POST" });
      if (!response.ok) {
        throw new Error(`${path} failed: ${response.status}`);
      }
      await load();
    } catch (err) {
      setError(err instanceof Error ? err.message : `${path} failed`);
    }
  }

  useEffect(() => {
    void load();
  }, []);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem(themeStorageKey, theme);
  }, [theme]);

  const counts = useMemo(() => {
    const result: Record<Health, number> = {
      healthy: 0,
      degraded: 0,
      unhealthy: 0,
      unknown: 0,
      error: 0
    };
    for (const service of summary?.services ?? []) {
      result[service.health] += 1;
    }
    return result;
  }, [summary]);

  const runtimeCounts = useMemo(() => {
    const result: Record<RuntimeFilter, number> = {
      all: summary?.services.length ?? 0,
      compose: 0,
      kubernetes: 0
    };
    for (const service of summary?.services ?? []) {
      if (service.runtime === "compose" || service.runtime === "kubernetes") {
        result[service.runtime] += 1;
      }
    }
    return result;
  }, [summary]);

  const services = useMemo(() => {
    const normalizedQuery = query.trim().toLowerCase();
    return (summary?.services ?? []).filter((service) => {
      if (selectedRuntime !== "all" && service.runtime !== selectedRuntime) {
        return false;
      }
      if (selectedHealth !== "all" && service.health !== selectedHealth) {
        return false;
      }
      if (normalizedQuery === "") {
        return true;
      }
      return searchableServiceText(service).includes(normalizedQuery);
    });
  }, [query, selectedHealth, selectedRuntime, summary]);

  const selectedService = useMemo(() => {
    return services.find((service) => service.id === selectedServiceId) ?? services[0] ?? null;
  }, [selectedServiceId, services]);

  const selectedStatuses = useMemo(() => {
    if (!selectedService || !summary) {
      return [];
    }
    return summary.statuses.filter((status) => status.serviceId === selectedService.id);
  }, [selectedService, summary]);

  const latestScan = summary?.scans[0] ?? null;
  const totalServices = summary?.services.length ?? 0;
  const selectedAccess = selectedService ? accessTargets(selectedService) : [];

  return (
    <main className="appShell">
      <header className="commandBar">
        <div className="brandBlock">
          <h1>GitOps Dashboard</h1>
          <div className="systemMeta" aria-label="Dashboard summary">
            <span>{summary?.repositories.length ?? 0} repos</span>
            <span>{totalServices} services</span>
            <span>{latestScan ? `last scan ${latestScan.status}` : "not scanned"}</span>
            <span>{summary?.generatedAt ? formatDate(summary.generatedAt) : "loading"}</span>
          </div>
        </div>
        <div className="actions">
          <label className="themeToggle">
            <input
              type="checkbox"
              checked={theme === "dark"}
              onChange={(event) => setTheme(event.target.checked ? "dark" : "light")}
              aria-label="Use dark theme"
            />
            <span className="switchTrack" aria-hidden="true">
              <span className="switchThumb" />
            </span>
            <span>Dark</span>
          </label>
          <button className="primaryAction" onClick={() => void post("/api/scan")}>Scan</button>
          <button onClick={() => void post("/api/monitor")}>Check Health</button>
          <button onClick={() => void load()}>Refresh</button>
        </div>
      </header>

      {error ? <section className="error">{error}</section> : null}

      <section className="metrics" aria-label="Service health">
        {healthOrder.map((health) => (
          <article key={health} className={`metric ${health}`}>
            <span>{stateLabel[health]}</span>
            <strong>{counts[health]}</strong>
          </article>
        ))}
      </section>

      <div className="workspace">
        <section className="panel inventoryPanel">
          <div className="panelHeader inventoryHeader">
            <div>
              <h2>Services</h2>
              <span className="panelCount">{services.length} of {totalServices}</span>
            </div>
            <div className="inventoryControls">
              <label className="searchField">
                <span className="srOnly">Search services</span>
                <input
                  type="search"
                  value={query}
                  onChange={(event) => setQuery(event.target.value)}
                  placeholder="Search services"
                />
              </label>
              <div className="segmented" aria-label="Runtime filter">
                {(Object.keys(runtimeLabels) as RuntimeFilter[]).map((runtime) => (
                  <button
                    key={runtime}
                    className={selectedRuntime === runtime ? "selected" : ""}
                    aria-pressed={selectedRuntime === runtime}
                    onClick={() => setSelectedRuntime(runtime)}
                  >
                    <span>{runtimeLabels[runtime]}</span>
                    <strong>{runtimeCounts[runtime]}</strong>
                  </button>
                ))}
              </div>
              <label className="healthFilter">
                <span className="srOnly">Health filter</span>
                <select
                  value={selectedHealth}
                  onChange={(event) => setSelectedHealth(event.target.value as HealthFilter)}
                >
                  <option value="all">All health</option>
                  {healthOrder.map((health) => (
                    <option key={health} value={health}>{stateLabel[health]}</option>
                  ))}
                </select>
              </label>
            </div>
          </div>
          <div className="serviceGrid serviceList">
            {services.map((service) => (
              <article
                className={`service ${selectedService?.id === service.id ? "selected" : ""}`}
                key={service.id}
                onClick={() => setSelectedServiceId(service.id)}
                onKeyDown={(event) => {
                  if (event.key === "Enter" || event.key === " ") {
                    event.preventDefault();
                    setSelectedServiceId(service.id);
                  }
                }}
                role="button"
                tabIndex={0}
              >
                <div className="serviceTitle">
                  <h3>{service.name}</h3>
                  <span>{service.runtime}</span>
                </div>
                <div className="accessCell">
                  {accessTargets(service).slice(0, 3).map((route) => (
                    <a
                      href={route.href}
                      key={route.href}
                      onClick={(event) => event.stopPropagation()}
                      rel="noreferrer"
                      target="_blank"
                    >
                      {route.label}
                    </a>
                  ))}
                  {accessTargets(service).length === 0 ? <span>No route discovered</span> : null}
                </div>
                <span className={`badge ${service.health}`}>{stateLabel[service.health]}</span>
              </article>
            ))}
            {services.length === 0 ? <p className="emptyState">No services match the current filters.</p> : null}
          </div>
        </section>

        {selectedService ? (
          <section className="panel detailPanel">
            <div className="panelHeader detailHeader">
              <div>
                <h2>{selectedService.name}</h2>
                <span className="panelCount">{selectedService.runtime} / {selectedService.repository}</span>
              </div>
              <span className={`badge ${selectedService.health}`}>{stateLabel[selectedService.health]}</span>
            </div>
            <div className="detailBody">
              <div className="accessList">
                {selectedAccess.length ? selectedAccess.map((route) => (
                  <a href={route.href} key={route.href} rel="noreferrer" target="_blank">
                    {route.label}
                  </a>
                )) : <p>No access route was discovered in Git.</p>}
              </div>
            </div>
            <div className="statusList">
              {selectedStatuses.length ? selectedStatuses.map((status) => (
                <div className="statusItem" key={`${status.serviceId}-${status.target}`}>
                  <span className={`badge ${status.health}`}>{stateLabel[status.health]}</span>
                  <strong>{status.target}</strong>
                  <span>{status.message || "no message"}</span>
                  <time>{status.checkedAt ? formatDate(status.checkedAt) : "not checked"}</time>
                </div>
              )) : <p>No live runtime status has been recorded for this service.</p>}
            </div>
          </section>
        ) : null}
      </div>
    </main>
  );
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
    ...service.exposure
  ].join(" ").toLowerCase();
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

createRoot(document.getElementById("root")!).render(<App />);
