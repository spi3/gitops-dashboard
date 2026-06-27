import React, { useEffect, useMemo, useState } from "react";
import { createRoot } from "react-dom/client";
import "./styles.css";

type Health = "healthy" | "degraded" | "unhealthy" | "unknown" | "error";

type Repository = {
  name: string;
  url: string;
  defaultRef: string;
  lastCommit: string;
  lastScanAt: string;
  status: string;
  error: string;
};

type Scan = {
  id: number;
  repository: string;
  status: string;
  commitSha: string;
  startedAt: string;
  finishedAt: string;
  error: string;
};

type Service = {
  id: string;
  name: string;
  repository: string;
  sourceCommit: string;
  runtime: string;
  kind: string;
  namespace: string;
  resourceName: string;
  sourcePath: string;
  environment: string;
  health: Health;
  images: string[];
  ports: string[];
  dependencies: string[];
  storage: string[];
  exposure: string[];
  configRefs: string[];
  warnings: string[];
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

const stateLabel: Record<Health, string> = {
  healthy: "Healthy",
  degraded: "Degraded",
  unhealthy: "Unhealthy",
  unknown: "Unknown",
  error: "Error"
};

function App() {
  const [summary, setSummary] = useState<DashboardSummary | null>(null);
  const [error, setError] = useState<string>("");
  const [selectedRuntime, setSelectedRuntime] = useState<string>("all");
  const [selectedServiceId, setSelectedServiceId] = useState<string>("");

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
    const response = await fetch(path, { method: "POST" });
    if (!response.ok) {
      setError(`${path} failed: ${response.status}`);
      return;
    }
    await load();
  }

  useEffect(() => {
    void load();
  }, []);

  const services = useMemo(() => {
    if (!summary) {
      return [];
    }
    if (selectedRuntime === "all") {
      return summary.services;
    }
    return summary.services.filter((service) => service.runtime === selectedRuntime);
  }, [selectedRuntime, summary]);

  const selectedService = useMemo(() => {
    if (!summary) {
      return null;
    }
    return services.find((service) => service.id === selectedServiceId) ?? services[0] ?? null;
  }, [selectedServiceId, services, summary]);

  const selectedStatuses = useMemo(() => {
    if (!selectedService || !summary) {
      return [];
    }
    return summary.statuses.filter((status) => status.serviceId === selectedService.id);
  }, [selectedService, summary]);

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

  return (
    <main>
      <header className="topbar">
        <div>
          <h1>GitOps Dashboard</h1>
          <p>Read-only inventory and health for GitOps repositories.</p>
        </div>
        <div className="actions">
          <button onClick={() => void post("/api/scan")}>Scan</button>
          <button onClick={() => void post("/api/monitor")}>Check Health</button>
          <button onClick={() => void load()}>Refresh</button>
        </div>
      </header>

      {error ? <section className="error">{error}</section> : null}

      <section className="metrics">
        {(Object.keys(counts) as Health[]).map((health) => (
          <article key={health} className={`metric ${health}`}>
            <span>{stateLabel[health]}</span>
            <strong>{counts[health]}</strong>
          </article>
        ))}
      </section>

      <section className="panel">
        <div className="panelHeader">
          <h2>Repositories</h2>
          <span>{summary?.repositories.length ?? 0}</span>
        </div>
        <div className="table">
          <div className="row head">
            <span>Name</span>
            <span>Ref</span>
            <span>Status</span>
            <span>Commit</span>
          </div>
          {(summary?.repositories ?? []).map((repo) => (
            <div className="row" key={repo.name}>
              <span>{repo.name}</span>
              <span>{repo.defaultRef || "default"}</span>
              <span>{repo.error || repo.status || "unknown"}</span>
              <span>{repo.lastCommit || "not scanned"}</span>
            </div>
          ))}
        </div>
      </section>

      <section className="panel">
        <div className="panelHeader">
          <h2>Scan History</h2>
          <span>{summary?.scans.length ?? 0}</span>
        </div>
        <div className="table scans">
          <div className="row scanRow head">
            <span>Repository</span>
            <span>Status</span>
            <span>Commit</span>
            <span>Finished</span>
          </div>
          {(summary?.scans ?? []).map((scan) => (
            <div className="row scanRow" key={scan.id}>
              <span>{scan.repository}</span>
              <span>{scan.error || scan.status}</span>
              <span>{scan.commitSha || "not recorded"}</span>
              <span>{scan.finishedAt || scan.startedAt}</span>
            </div>
          ))}
        </div>
      </section>

      <section className="panel">
        <div className="panelHeader">
          <h2>Services</h2>
          <select value={selectedRuntime} onChange={(event) => setSelectedRuntime(event.target.value)}>
            <option value="all">All runtimes</option>
            <option value="compose">Docker Compose</option>
            <option value="kubernetes">Kubernetes</option>
          </select>
        </div>
        <div className="serviceGrid">
          {services.map((service) => (
            <article
              className={`service ${selectedService?.id === service.id ? "selected" : ""}`}
              key={service.id}
              onClick={() => setSelectedServiceId(service.id)}
            >
              <div className="serviceTitle">
                <h3>{service.name}</h3>
                <span className={`badge ${service.health}`}>{stateLabel[service.health]}</span>
              </div>
              <dl>
                <dt>Repository</dt>
                <dd>{service.repository}</dd>
                <dt>Runtime</dt>
                <dd>{service.runtime}</dd>
                <dt>Environment</dt>
                <dd>{service.environment || "unknown"}</dd>
                <dt>Source</dt>
                <dd>{service.sourcePath}</dd>
              </dl>
              <p>{formatList(service.images, "No image declared")}</p>
              {service.configRefs.length ? <p>Config: {service.configRefs.join(", ")}</p> : null}
              {service.warnings.length ? (
                <ul>
                  {service.warnings.map((warning) => <li key={warning}>{warning}</li>)}
                </ul>
              ) : null}
            </article>
          ))}
        </div>
      </section>

      {selectedService ? (
        <section className="panel">
          <div className="panelHeader">
            <h2>Service Detail</h2>
            <span className={`badge ${selectedService.health}`}>{stateLabel[selectedService.health]}</span>
          </div>
          <div className="detailGrid">
            <dl>
              <dt>Name</dt>
              <dd>{selectedService.name}</dd>
              <dt>Repository</dt>
              <dd>{selectedService.repository}</dd>
              <dt>Commit</dt>
              <dd>{selectedService.sourceCommit || "not scanned"}</dd>
              <dt>Kind</dt>
              <dd>{selectedService.kind || selectedService.runtime}</dd>
              <dt>Namespace</dt>
              <dd>{selectedService.namespace || "default"}</dd>
              <dt>Resource</dt>
              <dd>{selectedService.resourceName || selectedService.name}</dd>
            </dl>
            <dl>
              <dt>Images</dt>
              <dd>{formatList(selectedService.images, "none")}</dd>
              <dt>Ports</dt>
              <dd>{formatList(selectedService.ports, "none")}</dd>
              <dt>Dependencies</dt>
              <dd>{formatList(selectedService.dependencies, "none")}</dd>
              <dt>Storage</dt>
              <dd>{formatList(selectedService.storage, "none")}</dd>
              <dt>Exposure</dt>
              <dd>{formatList(selectedService.exposure, "none")}</dd>
              <dt>Config</dt>
              <dd>{formatList(selectedService.configRefs, "none")}</dd>
            </dl>
          </div>
          <div className="statusList">
            {selectedStatuses.length ? selectedStatuses.map((status) => (
              <div className="statusItem" key={`${status.serviceId}-${status.target}`}>
                <span className={`badge ${status.health}`}>{stateLabel[status.health]}</span>
                <strong>{status.target}</strong>
                <span>{status.message || "no message"}</span>
                <time>{status.checkedAt || "not checked"}</time>
              </div>
            )) : <p>No live runtime status has been recorded for this service.</p>}
          </div>
        </section>
      ) : null}
    </main>
  );
}

function formatList(values: string[], fallback: string) {
  return values.length ? values.join(", ") : fallback;
}

createRoot(document.getElementById("root")!).render(<App />);
