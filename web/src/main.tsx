import React, { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createRoot } from "react-dom/client";
import "@fontsource-variable/bricolage-grotesque/index.css";
import "@fontsource-variable/jetbrains-mono/index.css";
import "./styles.css";

import { fetchSummary, runDashboardAction, setMonitorNotApplicableRequest } from "./api";
import { AgentsView } from "./components/AgentsView";
import { ServicesView } from "./components/ServicesView";
import { AgentDrawer, ServiceDrawer } from "./components/drawers";
import { agentsTabHash, attentionStates, refreshIntervalMs, themeStorageKey } from "./constants";
import {
  agentConnection,
  groupByEnvironment,
  initialTheme,
  latestAgentReportTime,
  latestCheckTime,
  monitorOverrideKey,
  overallAgentStatus,
  overallStatus,
  searchableServiceText,
  tabFromHash
} from "./selectors";
import type { DashboardSummary, Tab, Theme, UptimeStat } from "./types";

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
      setSummary(await fetchSummary());
      setError("");
    } catch (err) {
      setError(err instanceof Error ? err.message : "failed to load dashboard");
    }
  }, []);

  const trigger = useCallback(async (action: "scan" | "monitor") => {
    setBusyAction(action);
    try {
      await runDashboardAction(action);
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
      await setMonitorNotApplicableRequest(serviceId, target, notApplicable);
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
  const buildVersion = summary?.version ?? null;

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
        <ServicesView
          attentionCount={attentionCount}
          attentionOnly={attentionOnly}
          buildVersion={buildVersion}
          busyAction={busyAction}
          error={error}
          filteredCount={filtered.length}
          groups={groups}
          lastChecked={lastChecked}
          latestScan={latestScan}
          onAttentionOnlyChange={setAttentionOnly}
          onClearFilters={() => {
            setQuery("");
            setAttentionOnly(false);
          }}
          onOpenService={setSelectedServiceId}
          onQueryChange={setQuery}
          onRetry={() => void load()}
          onTrigger={(action) => void trigger(action)}
          overall={overall}
          query={query}
          repositoryCount={repositoryCount}
          services={services}
          uptimeByService={uptimeByService}
        />
      ) : (
        <AgentsView
          agentOverall={agentOverall}
          agents={agents}
          latestAgentReport={latestAgentReport}
          onOpenAgent={setSelectedAgentTarget}
        />
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

createRoot(document.getElementById("root")!).render(<App />);
