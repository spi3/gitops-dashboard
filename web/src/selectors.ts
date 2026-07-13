import {
  agentConnectedThresholdMs,
  agentConnectionTone,
  agentStaleThresholdMs,
  agentsTabHash,
  environmentSortOrder,
  routeTargetPrefix,
  routesTarget,
  serviceSortOrder,
  statusWord,
  tallyWord,
  themeStorageKey
} from "./constants";
import type { AgentConnection, AgentInfo, BuildInfo, ContainerStatus, EnvironmentGroup, Health, ImageReference, MonitorTargetDetail, MonitorTargetKind, ObservedImage, Service, StatusResult, Tab, TargetSample, Theme, Tone, UptimeSample, UptimeStat } from "./types";

export function tabFromHash(): Tab {
  return window.location.hash === agentsTabHash ? "agents" : "services";
}

export function agentConnection(agent: AgentInfo): AgentConnection {
  if (!agent.lastSeenAt) {
    return "never";
  }
  const seenAt = new Date(agent.lastSeenAt).getTime();
  if (Number.isNaN(seenAt)) {
    return "never";
  }
  const staleAfterAt = new Date(agent.staleAfter ?? "").getTime();
  const staleAfterMs = Number.isNaN(staleAfterAt)
    ? agentStaleThresholdMs
    : Math.max(0, staleAfterAt - seenAt);
  const connectedThresholdMs = staleAfterMs > 0 ? staleAfterMs / 2 : agentConnectedThresholdMs;
  const elapsedMs = Date.now() - seenAt;
  if (elapsedMs < connectedThresholdMs) {
    return "connected";
  }
  if (elapsedMs < staleAfterMs) {
    return "stale";
  }
  return "offline";
}

export function isContainerRunning(container: ContainerStatus): boolean {
  return container.state === "running" && container.health !== "unhealthy" && container.health !== "starting";
}

export function agentCardTone(connection: AgentConnection, containers: ContainerStatus[]): Health {
  if (connection === "connected") {
    return containers.some((container) => !isContainerRunning(container)) ? "degraded" : "healthy";
  }
  return agentConnectionTone[connection];
}

export function overallAgentStatus(agents: AgentInfo[]): { tone: Tone; sentence: string } {
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

export function latestAgentReportTime(agents: AgentInfo[]): string {
  let latest = "";
  for (const agent of agents) {
    if (agent.lastSeenAt && agent.lastSeenAt > latest) {
      latest = agent.lastSeenAt;
    }
  }
  return latest;
}

export function containerTally(containers: ContainerStatus[]): string {
  if (containers.length === 0) {
    return "no containers reported";
  }
  const running = containers.filter(isContainerRunning).length;
  return `${running} of ${containers.length} running`;
}

export function containerTone(container: ContainerStatus): Health {
  if (container.health === "starting") {
    return "degraded";
  }
  if (!isContainerRunning(container)) {
    if (container.state === "paused" || container.state === "restarting") {
      return "degraded";
    }
    return "unhealthy";
  }
  return "healthy";
}

export function containerWord(container: ContainerStatus): string {
  if (container.state !== "running") {
    return titleize(container.state || "unknown");
  }
  if (container.health === "unhealthy") {
    return "Unhealthy";
  }
  if (container.health === "starting") {
    return "Starting";
  }
  if (container.health === "none") {
    return "No healthcheck";
  }
  return "Running";
}

export function sortContainers(containers: ContainerStatus[]): ContainerStatus[] {
  return [...containers].sort((left, right) => {
    const leftRunning = isContainerRunning(left) ? 1 : 0;
    const rightRunning = isContainerRunning(right) ? 1 : 0;
    return leftRunning - rightRunning || left.name.localeCompare(right.name);
  });
}

export function overallStatus(services: Service[]): { tone: Tone; sentence: string } {
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

export function countByHealth(services: Service[]) {
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

export function groupByEnvironment(services: Service[]): EnvironmentGroup[] {
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

export function aggregateUptimeSamples(stats: UptimeStat[]): UptimeSample[] {
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

export function aggregateSampleHealth(healths: Health[]): Health {
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

export function latestSampleTime(samples: TargetSample[]): string {
  return samples.reduce((latest, { sample }) => (
    sample.checkedAt > latest ? sample.checkedAt : latest
  ), "");
}

export function aggregateSampleMessage(samples: TargetSample[]): string {
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

export function worstPercent(stats: UptimeStat[]): number | null {
  const measured = stats.filter((stat) => stat.checkCount > 0);
  if (measured.length === 0) {
    return null;
  }
  return measured.reduce((worst, stat) => Math.min(worst, stat.uptimePercent), 100);
}

export function latestCheckTime(uptime: UptimeStat[], statuses: StatusResult[]): string {
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

export function monitorTargetDetails(service: Service, statuses: StatusResult[], uptime: UptimeStat[]): MonitorTargetDetail[] {
  const byTarget = new Map<string, MonitorTargetDetail>();
  const routeTargets = monitorRouteTargets(service);
  const currentRouteTargetKeys = new Set(routeTargets.map((route) => routeTargetForUrl(route)));
  const ensureDetail = (target: string, serviceRoutes = false) => {
    const key = target;
    let detail = byTarget.get(key);
    if (!detail) {
      const presentation = monitorTargetPresentation(target, serviceRoutes);
      detail = { target, ...presentation, status: null, uptime: null };
      byTarget.set(key, detail);
    } else if (serviceRoutes) {
      detail.label = "All routes";
      detail.kind = "service_routes";
    }
    return detail;
  };

  for (const stat of uptime) {
    ensureDetail(stat.target).uptime = stat;
  }
  for (const status of statuses) {
    const detail = ensureDetail(status.target);
    detail.target = status.target;
    detail.status = status;
  }

  const hasCurrentRouteRows = Array.from(byTarget.values()).some((target) => (
    target.kind === "route" && currentRouteTargetKeys.has(target.target)
  ));
  const parentRouteStatus = statuses.find((status) => status.target === routesTarget);
  const hasServiceRoutesParent = isServiceRoutesParentStatus(parentRouteStatus, routeTargets.length > 0, hasCurrentRouteRows);
  if (hasServiceRoutesParent) {
    ensureDetail(routesTarget, true);
  } else if (routeTargets.length > 0 && hasCurrentRouteRows && !byTarget.has(routesTarget)) {
    ensureDetail(routesTarget, true);
  }

  if (routeTargets.length > 0 && (hasCurrentRouteRows || hasServiceRoutesParent)) {
    for (const route of routeTargets) {
      if (!byTarget.has(routeTargetForUrl(route))) {
        ensureDetail(routeTargetForUrl(route));
      }
    }
  }

  const routeOrder = routeMonitorOrder(service);
  return Array.from(byTarget.values()).sort((left, right) => compareMonitorTargets(left, right, routeOrder));
}

export function isServiceRoutesParentStatus(status: StatusResult | undefined, hasConfiguredRoutes: boolean, hasCurrentRouteRows: boolean): boolean {
  if (!hasConfiguredRoutes || !status || status.target !== routesTarget || hasCurrentRouteRows) {
    return false;
  }
  if (status.health === "not_applicable") {
    return true;
  }
  return status.health === "unknown" && status.message === "monitor enabled; waiting for next check";
}

export function targetDetailMeta(target: MonitorTargetDetail): string {
  if (isPolicyBlocked(target.status)) {
    return "blocked by policy";
  }
  if (target.status?.health === "not_applicable") {
    return "not applicable";
  }
  if (target.uptime && target.uptime.checkCount > 0) {
    return `${target.uptime.uptimePercent}% · ${target.uptime.checkCount} ${plural(target.uptime.checkCount, "check")} · 24h`;
  }
  if (target.status) {
    return statusWord[target.status.health];
  }
  if (target.kind === "service_routes") {
    return "all routes";
  }
  return "no checks yet";
}

export function isPolicyBlocked(status: StatusResult | null | undefined): boolean {
  return status?.health === "not_applicable" && status.message.startsWith("blocked by policy");
}

export function targetBlockClass(target: MonitorTargetDetail, ignored: boolean): string {
  return [
    "targetBlock",
    target.kind === "service_routes" ? "allRoutesTarget" : "",
    target.kind === "route" ? "routeTarget" : "",
    ignored ? "notApplicable" : ""
  ].filter(Boolean).join(" ");
}

export function monitorTargetPresentation(target: string, serviceRoutes = false): Pick<MonitorTargetDetail, "label" | "kind"> {
  if (target === routesTarget && serviceRoutes) {
    return { label: "All routes", kind: "service_routes" };
  }
  if (isPerRouteTarget(target)) {
    return { label: routeUrlForTarget(target), kind: "route" };
  }
  return { label: target, kind: "target" };
}

export function compareMonitorTargets(
  left: MonitorTargetDetail,
  right: MonitorTargetDetail,
  routeOrder: Map<string, number>
): number {
  const kindOrder: Record<MonitorTargetKind, number> = {
    service_routes: 0,
    route: 1,
    target: 2
  };
  const leftKind = kindOrder[left.kind];
  const rightKind = kindOrder[right.kind];
  if (leftKind !== rightKind) {
    return leftKind - rightKind;
  }
  if (left.kind === "route" && right.kind === "route") {
    const leftOrder = routeOrder.get(left.target) ?? Number.MAX_SAFE_INTEGER;
    const rightOrder = routeOrder.get(right.target) ?? Number.MAX_SAFE_INTEGER;
    if (leftOrder !== rightOrder) {
      return leftOrder - rightOrder;
    }
  }
  return left.label.localeCompare(right.label);
}

export function routeMonitorOrder(service: Service): Map<string, number> {
  const order = new Map<string, number>();
  monitorRouteTargets(service).forEach((route, index) => {
    order.set(routeTargetForUrl(route), index);
  });
  return order;
}

export function isPerRouteTarget(target: string): boolean {
  return target.startsWith(routeTargetPrefix);
}

export function routeUrlForTarget(target: string): string {
  const route = target.startsWith(`${routeTargetPrefix} `)
    ? target.slice(routeTargetPrefix.length + 1)
    : target.slice(routeTargetPrefix.length);
  return route || "Route";
}

export function routeTargetForUrl(url: string): string {
  return `${routeTargetPrefix} ${url}`;
}

export function monitorRouteTargets(service: Service): string[] {
  return service.monitorRoutes ?? [];
}

export function monitorOverrideKey(serviceId: string, target: string): string {
  return `${serviceId}\u0000${target}`;
}

export function stripLabel(samples: UptimeSample[]): string {
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

export function initialTheme(): Theme {
  const savedTheme = window.localStorage.getItem(themeStorageKey);
  if (savedTheme === "light" || savedTheme === "dark") {
    return savedTheme;
  }
  return window.matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light";
}

export function searchableServiceText(service: Service) {
  return [
    service.name,
    service.repository,
    service.runtime,
    service.environment,
    service.kind,
    service.imageVersionState ?? "",
    ...service.images,
    ...service.exposure
  ].join(" ").toLowerCase();
}

export function buildVersionLabel(version: BuildInfo): string {
  const commit = version.commit && version.commit !== "unknown" ? version.commit.slice(0, 12) : "unknown";
  const buildDate = version.buildDate && version.buildDate !== "unknown" ? version.buildDate : "unknown";
  return `GitOps Dashboard ${version.version || "dev"} · ${commit} · built ${buildDate}`;
}

export function imageRefLabel(ref: ImageReference): string {
  if (ref.original) {
    return ref.original;
  }
  const base = [ref.registry, ref.repository].filter(Boolean).join("/");
  if (ref.digest) {
    return `${base}@${ref.digest}`;
  }
  if (ref.tag) {
    return `${base}:${ref.tag}`;
  }
  return base || "image";
}

export function observedImageLabel(image: ObservedImage): string {
  const reference = imageRefLabel(image.reference);
  const repoDigestLabels = (image.repoDigests ?? []).map(repoDigestLabel).filter(Boolean);
  if (reference !== "image") {
    return repoDigestLabels.length > 0 ? `${reference} · ${repoDigestLabels.join(", ")}` : reference;
  }
  if (repoDigestLabels.length > 0) {
    return (image.repoDigests ?? []).map(repoDigestReferenceLabel).filter(Boolean).join(", ");
  }
  return image.imageId || "reported without reference";
}

export function observedImageTitle(image: ObservedImage): string {
  const reference = imageRefLabel(image.reference);
  const parts = reference !== "image" ? [reference] : [];
  const repoDigests = (image.repoDigests ?? []).map(imageRefLabel).filter(Boolean);
  if (repoDigests.length > 0) {
    parts.push(`repo digests: ${repoDigests.join(", ")}`);
  }
  if (image.imageId) {
    parts.push(`image ID: ${image.imageId}`);
  }
  return parts.join(" · ") || observedImageLabel(image);
}

export function repoDigestLabel(ref: ImageReference): string {
  if (ref.digest) {
    return shortDigest(ref.digest);
  }
  return imageRefLabel(ref);
}

export function repoDigestReferenceLabel(ref: ImageReference): string {
  const base = [ref.registry, ref.repository].filter(Boolean).join("/");
  if (ref.digest) {
    return base ? `${base}@${shortDigest(ref.digest)}` : shortDigest(ref.digest);
  }
  return imageRefLabel(ref);
}

export function shortDigest(digest: string): string {
  const [algorithm, value] = digest.split(":", 2);
  if (algorithm && value && value.length > 8) {
    return `${algorithm}:${value.slice(0, 8)}…`;
  }
  return digest;
}

export function accessTargets(service: Service) {
  const targets = new Map<string, { href: string; label: string }>();
  for (const route of service.exposure.map(stripRouteUserinfo).filter(isAccessRoute)) {
    const href = hrefForRoute(route);
    const key = href.replace(/\/$/, "");
    if (!targets.has(key)) {
      targets.set(key, { href, label: labelForRoute(route) });
    }
  }
  return Array.from(targets.values());
}

export function isAccessRoute(value: string) {
  value = stripRouteUserinfo(value);
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

export function hrefForRoute(value: string) {
  value = stripRouteUserinfo(value);
  if (/^(https?|ssh):\/\//.test(value)) {
    return value;
  }
  const host = hostForRoute(value);
  const scheme = host.endsWith(".lan") || /^\d{1,3}(\.\d{1,3}){3}$/.test(host) ? "http" : "https";
  return `${scheme}://${value}`;
}

export function labelForRoute(value: string) {
  value = stripRouteUserinfo(value);
  return value.replace(/^(https?|ssh):\/\//, "").replace(/\/$/, "");
}

export function hostForRoute(value: string) {
  value = stripRouteUserinfo(value);
  if (/^(https?|ssh):\/\//.test(value)) {
    try {
      return new URL(value).hostname;
    } catch {
      return "";
    }
  }
  return value.split(/[/:]/, 1)[0] ?? "";
}

export function stripRouteUserinfo(value: string) {
  return value.replace(/^([a-z][a-z0-9+.-]*:\/\/)([^/?#]*@)([^/?#]*)(.*)$/i, "$1$3$4");
}

export function isClusterInternalHost(host: string) {
  return host.endsWith(".svc") || host.includes(".svc.") || host.endsWith(".cluster.local");
}

export function environmentLabel(value: string) {
  return titleize(value || "unassigned");
}

export function runtimeLabel(value: string) {
  if (value === "kubernetes") {
    return "Kubernetes";
  }
  if (value === "host") {
    return "Host";
  }
  return titleize(value || "other");
}

export function titleize(value: string) {
  return value
    .split(/[-_\s]+/)
    .filter(Boolean)
    .map((part) => `${part.charAt(0).toUpperCase()}${part.slice(1)}`)
    .join(" ") || "Unassigned";
}

export function plural(count: number, singular: string, pluralForm?: string) {
  return count === 1 ? singular : pluralForm ?? `${singular}s`;
}

export function relativeTime(value: string) {
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

export function formatDate(value: string) {
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
