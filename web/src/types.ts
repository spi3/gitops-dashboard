export type Health = "healthy" | "degraded" | "unhealthy" | "unknown" | "error" | "not_applicable";
export type Theme = "light" | "dark";
export type Tone = "steady" | "pending" | "watch" | "alert";
export type ImageVersionState = "matching" | "mismatched" | "unknown" | "mutable";

export type Repository = {
  name: string;
  status: string;
  lastScanAt: string;
};

export type Scan = {
  id: number;
  repository: string;
  status: string;
  finishedAt: string;
};

export type BuildInfo = {
  version: string;
  commit: string;
  buildDate: string;
};

export type ImageReference = {
  original: string;
  registry: string;
  repository: string;
  tag: string;
  digest: string;
};

export type ObservedImage = {
  target: string;
  runtime: string;
  reference: ImageReference;
  imageId: string;
  repoDigests: ImageReference[];
};

export type ImageVersionCheck = {
  desired: ImageReference;
  observed?: ObservedImage;
  state: ImageVersionState;
  message: string;
};

export type Service = {
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
  desiredImages?: ImageReference[];
  imageVersionState?: ImageVersionState;
  imageVersionChecks?: ImageVersionCheck[];
  dependencies: string[];
  exposure: string[];
  monitorRoutes?: string[];
};

export type StatusResult = {
  serviceId: string;
  target: string;
  health: Health;
  message: string;
  checkedAt: string;
  observedImages?: ObservedImage[];
};

export type UptimeSample = {
  health: Health;
  checkedAt: string;
  message: string;
};

export type UptimeStat = {
  serviceId: string;
  target: string;
  uptimePercent: number;
  checkCount: number;
  samples: UptimeSample[];
};

export type TargetSample = {
  target: string;
  sample: UptimeSample;
};

export type MonitorTargetDetail = {
  target: string;
  label: string;
  kind: MonitorTargetKind;
  status: StatusResult | null;
  uptime: UptimeStat | null;
};

export type MonitorTargetKind = "service_routes" | "route" | "target";

export type ContainerStatus = {
  id: string;
  name: string;
  image: string;
  state: string;
  status: string;
  health: string;
  restartCount: number;
};

export type AgentInfo = {
  target: string;
  lastSeenAt: string;
  configured: boolean;
  containers: ContainerStatus[];
};

export type DashboardSummary = {
  repositories: Repository[];
  services: Service[];
  scans: Scan[];
  statuses: StatusResult[];
  uptime?: UptimeStat[];
  agents?: AgentInfo[];
  version?: BuildInfo;
  generatedAt: string;
};

export type EnvironmentGroup = {
  environment: string;
  services: Service[];
  upCount: number;
};

export type Tab = "services" | "agents";
export type AgentConnection = "connected" | "stale" | "offline" | "never";
