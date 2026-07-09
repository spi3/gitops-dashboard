import type { AgentConnection, Health, ImageVersionState } from "./types";

export const statusWord: Record<Health, string> = {
  healthy: "Up",
  degraded: "Degraded",
  unhealthy: "Down",
  unknown: "No data",
  error: "Check failed",
  not_applicable: "Not applicable"
};

export const tallyWord: Record<Health, string> = {
  healthy: "up",
  degraded: "degraded",
  unhealthy: "down",
  unknown: "unchecked",
  error: "failed",
  not_applicable: "not applicable"
};

export const imageVersionWord: Record<ImageVersionState, string> = {
  matching: "Image matches",
  mismatched: "Image drift",
  unknown: "Image unknown",
  mutable: "Mutable image"
};

export const attentionStates: Health[] = ["degraded", "unhealthy", "error"];

export const serviceSortOrder: Record<Health, number> = {
  error: 0,
  unhealthy: 1,
  degraded: 2,
  healthy: 3,
  unknown: 4,
  not_applicable: 5
};

export const environmentSortOrder: Record<string, number> = {
  production: 0,
  staging: 1,
  homelab: 2,
  development: 3,
  testing: 4,
  local: 5,
  unassigned: 6
};

export const agentConnectionWord: Record<AgentConnection, string> = {
  connected: "Connected",
  stale: "Stale",
  offline: "Offline",
  never: "Never connected"
};

export const agentConnectionTone: Record<AgentConnection, Health> = {
  connected: "healthy",
  stale: "degraded",
  offline: "unhealthy",
  never: "unknown"
};

export const agentConnectedThresholdMs = 120_000;
export const agentStaleThresholdMs = 600_000;
export const agentsTabHash = "#/agents";

export const tileSlots = 28;
export const drawerSlots = 40;
export const refreshIntervalMs = 30_000;
export const themeStorageKey = "gitops-dashboard-theme";
export const routesTarget = "routes";
export const routeTargetPrefix = `${routesTarget}:`;
