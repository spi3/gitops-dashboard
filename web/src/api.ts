import type { DashboardSummary } from "./types";

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
    throw new Error(`/api/${action} failed: ${response.status}`);
  }
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
