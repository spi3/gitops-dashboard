import type { DashboardActionStatus, DashboardSummary } from "./types";

const stateChangingRequestHeaders = { "X-GitOps-Dashboard-CSRF": "1" };

export async function fetchSummary(): Promise<DashboardSummary> {
  const response = await fetch("/api/summary");
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail ? `summary request failed: ${response.status}: ${detail}` : `summary request failed: ${response.status}`);
  }
  return await response.json() as DashboardSummary;
}

export async function runDashboardAction(action: "scan" | "monitor"): Promise<void> {
  const response = await fetch(`/api/${action}`, {
    method: "POST",
    headers: stateChangingRequestHeaders
  });
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail ? `/api/${action} failed: ${response.status}: ${detail}` : `/api/${action} failed: ${response.status}`);
  }
  const actionStatus = await response.json() as DashboardActionStatus;
  await waitForDashboardAction(actionStatus);
}

export async function setMonitorNotApplicableRequest(serviceId: string, target: string, notApplicable: boolean): Promise<void> {
  const response = await fetch("/api/monitor-overrides", {
    method: "POST",
    headers: { ...stateChangingRequestHeaders, "Content-Type": "application/json" },
    body: JSON.stringify({ serviceId, target, notApplicable })
  });
  if (!response.ok) {
    throw new Error(`/api/monitor-overrides failed: ${response.status}`);
  }
}

async function waitForDashboardAction(actionStatus: DashboardActionStatus): Promise<void> {
  let current = actionStatus;
  while (current.status === "running") {
    await delay(1000);
    current = await fetchDashboardAction(current.id);
  }
  if (current.status === "error") {
    throw new Error(current.error ? `/api/${current.action} failed: ${current.error}` : `/api/${current.action} failed`);
  }
}

async function fetchDashboardAction(id: string): Promise<DashboardActionStatus> {
  const response = await fetch(`/api/actions/${encodeURIComponent(id)}`);
  if (!response.ok) {
    const detail = (await response.text()).trim();
    throw new Error(detail ? `action status failed: ${response.status}: ${detail}` : `action status failed: ${response.status}`);
  }
  return await response.json() as DashboardActionStatus;
}

function delay(ms: number): Promise<void> {
  return new Promise((resolve) => window.setTimeout(resolve, ms));
}
