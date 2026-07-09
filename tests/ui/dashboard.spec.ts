import { expect, test, type Page } from "@playwright/test";
import { spawn, spawnSync, type ChildProcessWithoutNullStreams } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

let tempRoot = "";
let baseURL = "";
let server: ChildProcessWithoutNullStreams | null = null;
let fakeDocker: Server | null = null;
let serverLogs = "";

test.beforeAll(async () => {
  tempRoot = mkdtempSync(path.join(tmpdir(), "gitops-dashboard-ui-"));
  const fixtureRepo = path.join(tempRoot, "fixture");
  const dataDir = path.join(tempRoot, "data");
  const configPath = path.join(tempRoot, "config.yaml");

  createFixtureRepo(fixtureRepo);
  mkdirSync(dataDir, { recursive: true });

  const port = await freePort();
  const dockerPort = await freePort();
  fakeDocker = createFakeDockerServer();
  await listen(fakeDocker, dockerPort);

  baseURL = `http://127.0.0.1:${port}`;
  writeFileSync(configPath, [
    "server:",
    `  listen: "127.0.0.1:${port}"`,
    `  dataDir: "${dataDir}"`,
    `  repoCacheDir: "${path.join(dataDir, "repos")}"`,
    "auth:",
    "  mode: dev-no-auth",
    "monitoring:",
    "  defaultInterval: 30s",
    "repositories:",
    "  - name: fixture",
    `    url: "file://${fixtureRepo}"`,
    "    defaultRef: main",
    "runtime:",
    "  docker:",
    "    - name: local-docker",
    `      host: "http://127.0.0.1:${dockerPort}"`,
    "      interval: 5m",
    "  kubernetes: []",
    ""
  ].join("\n"));

  server = spawn(path.join(repoRoot, "gitops-dashboard"), ["-config", configPath], {
    cwd: repoRoot
  });
  server.stdout.on("data", (chunk) => {
    serverLogs += chunk.toString();
  });
  server.stderr.on("data", (chunk) => {
    serverLogs += chunk.toString();
  });
  await waitForServer(baseURL);
});

test.afterAll(() => {
  if (server) {
    server.kill("SIGTERM");
  }
  if (fakeDocker) {
    fakeDocker.close();
  }
  if (tempRoot) {
    rmSync(tempRoot, { recursive: true, force: true });
  }
});

test("verifies the full dashboard workflow against the real server", async ({ page }) => {
  const browserIssues: string[] = [];
  let allowExpectedNetworkError = false;
  page.on("console", (message) => {
    if (
      allowExpectedNetworkError &&
      message.type() === "error" &&
      message.text().includes("Failed to load resource")
    ) {
      return;
    }
    if (message.type() === "warning" || message.type() === "error") {
      browserIssues.push(`${message.type()}: ${message.text()}`);
    }
  });
  page.on("pageerror", (error) => {
    browserIssues.push(`pageerror: ${error.message}`);
  });

  await page.goto(baseURL);
  await expect(page.getByRole("heading", { name: "GitOps Dashboard" })).toBeVisible();
  await expect(page.locator(".sentence")).toHaveText("Waiting for the first scan");
  await expect(page.getByText("Nothing here yet")).toBeVisible();

  await expect(page.locator("html")).toHaveAttribute("data-theme", /light|dark/);
  const themeButton = page.getByRole("button", { name: "Use dark theme" });
  await themeButton.click();
  const flipped = await page.locator("html").getAttribute("data-theme");
  await page.reload();
  await expect(page.locator("html")).toHaveAttribute("data-theme", flipped ?? "dark");

  await page.getByRole("button", { name: "Sync repos" }).click();
  await expect(page.locator(".sentence")).toHaveText("Waiting for live checks");
  await expect(page.getByRole("heading", { name: "Production" })).toBeVisible();
  await expect(page.locator(".tally")).toHaveText("0 of 2 up");

  const webTile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "web", exact: true }) });
  const apiTile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "api", exact: true }) });
  await expect(webTile).toBeVisible();
  await expect(apiTile).toBeVisible();
  await expect(webTile.locator(".stateWord")).toHaveText("No data");
  await expect(webTile.getByRole("link", { name: /web\.example\.test/ })).toHaveAttribute("href", "https://web.example.test");
  await expect(apiTile.getByRole("link", { name: /api\.example\.test/ })).toHaveAttribute("href", "https://api.example.test/");
  await expect(webTile.getByText("no checks yet")).toBeVisible();
  await expect(webTile.locator(".tick.empty")).toHaveCount(28);

  await page.locator(".searchField input").fill("api");
  await expect(webTile).toBeHidden();
  await expect(apiTile).toBeVisible();
  await page.locator(".searchField input").fill("");
  await expect(webTile).toBeVisible();

  await page.getByRole("button", { name: /Needs attention/ }).click();
  await expect(page.getByText("No services match")).toBeVisible();
  await page.getByRole("button", { name: "Clear filters" }).click();
  await expect(webTile).toBeVisible();

  await page.getByRole("button", { name: "Check now" }).click();
  await expect(page.locator(".sentence")).toHaveText("Everything checked is up");
  await expect(page.locator(".tally")).toHaveText("1 of 2 up");
  await expect(webTile.locator(".stateWord")).toHaveText("Up");
  await expect(webTile.locator(".imageBadge")).toHaveText("Image matches");
  await expect(webTile.locator(".tick.healthy")).toHaveCount(1);
  await expect(webTile.getByText(/100% · 24h/)).toBeVisible();
  await expect(apiTile.locator(".stateWord")).toHaveText("No data");

  await webTile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByRole("heading", { name: "web" })).toBeVisible();
  await expect(drawer.getByText(/Up · Compose · Production/)).toBeVisible();
  await expect(drawer.getByRole("link", { name: /web\.example\.test/ })).toHaveAttribute("href", "https://web.example.test");
  await expect(drawer.getByRole("heading", { name: "Image versions" })).toBeVisible();
  await expect(drawer.locator(".versionBlock .imageBadge")).toHaveText("Image matches");
  await expect(drawer.locator(".versionFacts")).toContainText("example/web:v1.0.0");
  await expect(drawer.locator(".versionFacts")).toContainText("local-docker");
  await expect(drawer.locator(".targetHead strong")).toHaveText("local-docker");
  await expect(drawer.locator(".targetHead span")).toHaveText(/100% · 1 check · 24h/);
  await expect(drawer.locator(".targetBlock .targetNote")).toContainText("Up 5 minutes");
  await expect(drawer.locator(".chip")).toHaveText("db");
  await expect(drawer.locator(".provenance").first()).toContainText("fixture · prod/compose.yaml @");
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();

  await expect(page.locator(".foot")).toContainText("last sync ok");
  await expect(page.locator(".foot")).toContainText("GitOps Dashboard");

  await page.route("**/api/summary", async (route) => {
    await route.fulfill({ status: 500, body: "summary unavailable" });
  });
  allowExpectedNetworkError = true;
  await page.getByRole("button", { name: "Check now" }).click();
  await expect(page.getByRole("alert")).toContainText("summary request failed: 500");
  await page.unroute("**/api/summary");
  await page.getByRole("button", { name: "Retry" }).click();
  await expect(page.getByRole("alert")).toBeHidden();
  allowExpectedNetworkError = false;

  expect(browserIssues).toEqual([]);
});

test("renders every supported health state in the browser", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithEveryHealthState())
    });
  });

  await page.goto(baseURL);
  await expect(page.locator(".sentence")).toHaveText("3 services need attention");
  const expectations: Array<[string, string]> = [
    ["healthy-service", "Up"],
    ["degraded-service", "Degraded"],
    ["unhealthy-service", "Down"],
    ["unknown-service", "No data"],
    ["error-service", "Check failed"],
    ["not_applicable-service", "Not applicable"]
  ];
  for (const [name, word] of expectations) {
    const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name, exact: true }) });
    await expect(tile.locator(".stateWord")).toHaveText(word);
  }

  await page.getByRole("button", { name: /Needs attention/ }).click();
  await expect(page.locator("article.tile")).toHaveCount(3);
});

test("renders image version states in tiles and details", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithImageVersionStates())
    });
  });

  await page.goto(baseURL);
  const expectations: Array<[string, string]> = [
    ["matching-image", "Image matches"],
    ["drifted-image", "Image drift"],
    ["unknown-image", "Image unknown"],
    ["mutable-image", "Mutable image"]
  ];
  for (const [name, word] of expectations) {
    const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name, exact: true }) });
    await expect(tile.locator(".imageBadge")).toHaveText(word);
  }

  const drifted = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "drifted-image", exact: true }) });
  await drifted.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.locator(".versionBlock.mismatched .imageBadge")).toHaveText("Image drift");
  await expect(drawer.locator(".versionFacts")).toContainText("example/drifted-image:v1.0.0");
  const observed = drawer.locator(".versionFacts div", { hasText: "Observed" }).locator("dd");
  await expect(observed).toHaveText("example/drifted-image:v2.0.0 · sha256:feedface…");
  await expect(observed).toHaveAttribute("title", /example\/drifted-image@sha256:feedfacecafebeef00112233445566778899aabbccddeeff/);
  await expect(page.locator(".foot")).toContainText("GitOps Dashboard v1.2.3 · abc123def456 · built 2026-07-08T12:34:56Z");
});

test("renders uptime history and drawer details from the summary", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithUptimeHistory())
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "media", exact: true }) });
  await expect(tile.locator(".tick.healthy")).toHaveCount(5);
  await expect(tile.locator(".tick.unhealthy")).toHaveCount(1);
  await expect(tile.locator(".tick.empty")).toHaveCount(22);
  await expect(tile.getByText("83.3% · 24h")).toBeVisible();
  await expect(tile.locator(".pulseStrip")).toHaveAttribute("aria-label", /Last 6 checks/);

  await tile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.locator(".targetHead span")).toHaveText("83.3% · 6 checks · 24h");
  await expect(drawer.locator(".tick")).toHaveCount(40);
  await expect(drawer.getByRole("link", { name: /media\.example\.test/ })).toBeVisible();
  await drawer.getByRole("button", { name: "Close details" }).click();
  await expect(drawer).toBeHidden();
});

test("aggregates multi-target uptime on service tiles", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithMixedTargetUptime())
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "mixed-monitor", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Degraded");
  await expect(tile.locator(".tick.healthy")).toHaveCount(2);
  await expect(tile.locator(".tick.degraded")).toHaveCount(1);
  await expect(tile.locator(".tick.error")).toHaveCount(0);
  await expect(tile.locator(".tick.empty")).toHaveCount(25);
  await expect(tile.locator(".pulseStrip")).toHaveAttribute("aria-label", /1 degraded/);
});

test("can mark individual route monitor targets not applicable from the drawer", async ({ page }) => {
  const overrideRequests: MonitorOverrideRequest[] = [];
  const summary = summaryWithNotApplicableMonitor();
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summary)
    });
  });
  await page.route("**/api/monitor-overrides", async (route) => {
    const payload = route.request().postDataJSON() as MonitorOverrideRequest;
    overrideRequests.push(payload);
    applyMonitorOverride(summary, payload);
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" })
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "routed-app", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Up");
  await expect(tile.getByText("100% · 24h")).toBeVisible();

  await tile.click();
  const drawer = page.getByRole("dialog");
  const allRoutesTarget = drawer.locator(".targetBlock.allRoutesTarget");
  await expect(allRoutesTarget.locator(".targetHead strong")).toHaveText("All routes");
  await expect(allRoutesTarget.locator(".targetScope")).toHaveText("Service override");
  await expect(allRoutesTarget.getByRole("button", { name: "Mark not applicable" })).toBeVisible();
  await expect(drawer.getByRole("link", { name: /app\.example\.test:22/ })).toBeVisible();

  const routeTargets = drawer.locator(".targetBlock.routeTarget");
  await expect(routeTargets).toHaveCount(2);
  await expect(drawer.getByText("routes: https://app.example.test", { exact: true })).toHaveCount(0);
  await expect(drawer.getByText("routes: http://10.10.10.20", { exact: true })).toHaveCount(0);
  await expect(routeTargets.filter({ hasText: "ssh://app.example.test:22" })).toHaveCount(0);

  const routedTarget = routeTargets.filter({ hasText: "https://app.example.test" });
  await expect(routedTarget.locator(".targetHead strong")).toHaveText("https://app.example.test");
  await expect(routedTarget.locator(".targetScope")).toHaveText("Route");
  await expect(routedTarget.locator(".targetMeta")).toHaveText("100% · 3 checks · 24h");
  await expect(routedTarget.locator(".targetNote")).toContainText("Up");

  const directTarget = routeTargets.filter({ hasText: "http://10.10.10.20" });
  await expect(directTarget.locator(".targetHead strong")).toHaveText("http://10.10.10.20");
  await expect(directTarget.locator(".targetMeta")).toHaveText("not applicable");
  await expect(directTarget).toHaveClass(/notApplicable/);
  await directTarget.getByRole("button", { name: "Enable monitor" }).click();
  expect(overrideRequests[0]).toEqual({
    serviceId: "svc-routed",
    target: "routes: http://10.10.10.20",
    notApplicable: false
  });
  await expect(directTarget.locator(".targetMeta")).toHaveText("no checks yet");
  await expect(directTarget).not.toHaveClass(/notApplicable/);
  await expect(directTarget.getByRole("button", { name: "Mark not applicable" })).toBeVisible();

  await routedTarget.getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[1]).toEqual({
    serviceId: "svc-routed",
    target: "routes: https://app.example.test",
    notApplicable: true
  });
  await expect(routedTarget.locator(".targetMeta")).toHaveText("not applicable");
  await expect(routedTarget).toHaveClass(/notApplicable/);

  await allRoutesTarget.getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[2]).toEqual({
    serviceId: "svc-routed",
    target: "routes",
    notApplicable: true
  });
  await expect(allRoutesTarget.locator(".targetMeta")).toHaveText("not applicable");
  await expect(allRoutesTarget).toHaveClass(/notApplicable/);
  await expect(routeTargets).toHaveCount(2);
  await expect(routeTargets.filter({ hasText: "ssh://app.example.test:22" })).toHaveCount(0);
});

test("keeps all routes controls after re-enabling the all routes monitor", async ({ page }) => {
  const overrideRequests: MonitorOverrideRequest[] = [];
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithPostEnableAllRoutes())
    });
  });
  await page.route("**/api/monitor-overrides", async (route) => {
    overrideRequests.push(route.request().postDataJSON() as MonitorOverrideRequest);
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" })
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "all-routes-enabled", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("No data");

  await tile.click();
  const drawer = page.getByRole("dialog");
  const allRoutesTarget = drawer.locator(".targetBlock.allRoutesTarget");
  await expect(allRoutesTarget).toHaveCount(1);
  await expect(allRoutesTarget.locator(".targetHead strong")).toHaveText("All routes");
  await expect(allRoutesTarget.locator(".targetScope")).toHaveText("Service override");
  await expect(allRoutesTarget.locator(".targetMeta")).toHaveText("No data");
  await expect(allRoutesTarget.locator(".targetNote")).toContainText("monitor enabled; waiting for next check");
  await expect(allRoutesTarget.getByRole("button", { name: "Mark not applicable" })).toBeVisible();
  await expect(drawer.locator(".targetBlock .targetHead strong").filter({ hasText: /^routes$/ })).toHaveCount(0);

  const routeTargets = drawer.locator(".targetBlock.routeTarget");
  await expect(routeTargets).toHaveCount(2);
  await expect(routeTargets.filter({ hasText: "https://app.example.test" })).toHaveCount(1);
  await expect(routeTargets.filter({ hasText: "http://10.10.10.20" })).toHaveCount(1);

  await allRoutesTarget.getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[0]).toEqual({
    serviceId: "svc-all-routes-enabled",
    target: "routes",
    notApplicable: true
  });
});

test("keeps a literal routes monitor target out of the route override UI", async ({ page }) => {
  const overrideRequests: MonitorOverrideRequest[] = [];
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithLiteralRoutesMonitor())
    });
  });
  await page.route("**/api/monitor-overrides", async (route) => {
    overrideRequests.push(route.request().postDataJSON() as MonitorOverrideRequest);
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" })
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "literal-routes", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Up");

  await tile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByRole("link", { name: /literal\.example\.test:22/ })).toBeVisible();
  await expect(drawer.locator(".targetBlock.allRoutesTarget")).toHaveCount(0);
  await expect(drawer.locator(".targetBlock.routeTarget")).toHaveCount(0);

  const target = drawer.locator(".targetBlock").filter({ hasText: "routes" });
  await expect(target.locator(".targetHead strong")).toHaveText("routes");
  await expect(target.locator(".targetMeta")).toHaveText("100% · 2 checks · 24h");
  await target.getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[0]).toEqual({
    serviceId: "svc-literal-routes",
    target: "routes",
    notApplicable: true
  });
});

test("hides the all routes override when only stale route rows remain", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithStaleOnlyRouteRows())
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "stale-routes", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Down");

  await tile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByRole("link", { name: /stale\.example\.test:22/ })).toBeVisible();
  await expect(drawer.locator(".targetBlock.allRoutesTarget")).toHaveCount(0);
  await expect(drawer.getByText("All routes", { exact: true })).toHaveCount(0);

  const routeTargets = drawer.locator(".targetBlock.routeTarget");
  await expect(routeTargets).toHaveCount(1);
  await expect(routeTargets.locator(".targetHead strong")).toHaveText("https://old.example.test");
});

test("keys route controls from backend canonical monitor routes", async ({ page }) => {
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithCanonicalMonitorRoutes())
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "canonical-routes", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Up");

  await tile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.locator(".targetBlock.allRoutesTarget")).toHaveCount(1);
  const routeTargets = drawer.locator(".targetBlock.routeTarget");
  await expect(routeTargets).toHaveCount(1);
  await expect(routeTargets.locator(".targetHead strong")).toHaveText("https://app.example.test");
  await expect(routeTargets.getByText("HTTPS://APP.EXAMPLE.TEST", { exact: true })).toHaveCount(0);
  await expect(routeTargets.getByText("https://app.example.test:443", { exact: true })).toHaveCount(0);
});

test("keeps slash-distinct route controls and override targets", async ({ page }) => {
  const overrideRequests: MonitorOverrideRequest[] = [];
  await page.route("**/api/summary", async (route) => {
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify(summaryWithSlashDistinctRoutes())
    });
  });
  await page.route("**/api/monitor-overrides", async (route) => {
    overrideRequests.push(route.request().postDataJSON() as MonitorOverrideRequest);
    await route.fulfill({
      contentType: "application/json",
      body: JSON.stringify({ status: "ok" })
    });
  });

  await page.goto(baseURL);
  const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "slash-routes", exact: true }) });
  await expect(tile.locator(".stateWord")).toHaveText("Up");

  await tile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.locator(".targetBlock.allRoutesTarget")).toHaveCount(1);

  const routeTargets = drawer.locator(".targetBlock.routeTarget");
  await expect(routeTargets).toHaveCount(2);
  await expect(routeTargets.locator(".targetHead strong")).toHaveText([
    "https://app.example.test/admin",
    "https://app.example.test/admin/"
  ]);

  await routeTargets.nth(0).getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[0]).toEqual({
    serviceId: "svc-slash-routes",
    target: "routes: https://app.example.test/admin",
    notApplicable: true
  });

  await routeTargets.nth(1).getByRole("button", { name: "Mark not applicable" }).click();
  expect(overrideRequests[1]).toEqual({
    serviceId: "svc-slash-routes",
    target: "routes: https://app.example.test/admin/",
    notApplicable: true
  });
});

test.describe("agent report-driven compose service health", () => {
  const target = "agent-target";
  const token = "dashboard-agent-token";
  const serviceName = "worker";
  const serviceImage = "example/worker:v1.0.0";
  const healthyContainers = [{
    Id: "container-worker-1",
    Names: ["/worker-1"],
    Image: serviceImage,
    Labels: {
      "com.docker.compose.service": serviceName,
      "com.docker.compose.project": target
    },
    State: "running",
    Status: "Up 2 minutes",
    RestartCount: 0
  }];
  const unhealthyContainers = [{
    Id: "container-worker-1",
    Names: ["/worker-1"],
    Image: serviceImage,
    Labels: {
      "com.docker.compose.service": serviceName,
      "com.docker.compose.project": target
    },
    State: "running",
    Status: "Up 1 minute (unhealthy)",
    RestartCount: 3
  }];

  let tempRoot = "";
  let baseURL = "";
  let server: ChildProcessWithoutNullStreams | null = null;
  let fakeDocker: Server | null = null;
  let serverLogs = "";
  let agentConfigPath = "";
  let dockerPort = 0;
  let containerScenario: "unreported" | "healthy" | "unhealthy" = "unreported";
  let agentProcess: ChildProcessWithoutNullStreams | null = null;

  const currentContainers = () => {
    switch (containerScenario) {
      case "healthy":
        return healthyContainers;
      case "unhealthy":
        return unhealthyContainers;
      default:
        return [];
    }
  };

  const setContainerScenario = (scenario: "unreported" | "healthy" | "unhealthy") => {
    containerScenario = scenario;
  };

  test.beforeAll(async () => {
    tempRoot = mkdtempSync(path.join(tmpdir(), "gitops-dashboard-agent-ui-"));
    const fixtureRepo = path.join(tempRoot, "fixture");
    const dataDir = path.join(tempRoot, "data");
    const configPath = path.join(tempRoot, "config.yaml");
    agentConfigPath = path.join(tempRoot, "agent.yaml");

    createAgentFixtureRepo(fixtureRepo, target, serviceName, serviceImage);
    mkdirSync(dataDir, { recursive: true });

    const serverPort = await freePort();
    dockerPort = await freePort();
    fakeDocker = createDynamicFakeDockerServer(currentContainers);
    await listen(fakeDocker, dockerPort);

    baseURL = `http://127.0.0.1:${serverPort}`;
    writeFileSync(configPath, [
      "server:",
      `  listen: "127.0.0.1:${serverPort}"`,
      `  dataDir: "${dataDir}"`,
      `  repoCacheDir: "${path.join(dataDir, "repos")}"`,
      "auth:",
      "  mode: dev-no-auth",
      "  agent:",
      `    tokens: ["${token}"]`,
      "monitoring:",
      "  defaultInterval: 30s",
      "repositories:",
      "  - name: fixture",
      `    url: "file://${fixtureRepo}"`,
      "    defaultRef: main",
      "runtime:",
      "  docker:",
      `    - name: "${target}"`,
      "      kind: agent",
      "  kubernetes: []",
      ""
    ].join("\n"));
    writeFileSync(agentConfigPath, [
      "agent:",
      `  serverUrl: "ws://127.0.0.1:${serverPort}/api/agents/connect"`,
      `  target: "${target}"`,
      `  token: "${token}"`,
      "  interval: \"1s\"",
      "  docker:",
      `    host: "http://127.0.0.1:${dockerPort}"`,
      ""
    ].join("\n"));

    server = spawn(path.join(repoRoot, "gitops-dashboard"), ["-config", configPath], {
      cwd: repoRoot
    });
    server.stdout.on("data", (chunk) => {
      serverLogs += chunk.toString();
    });
    server.stderr.on("data", (chunk) => {
      serverLogs += chunk.toString();
    });
    await waitForServer(baseURL, () => serverLogs);
  });

  test.afterAll(() => {
    if (agentProcess) {
      agentProcess.kill("SIGTERM");
    }
    if (server) {
      server.kill("SIGTERM");
    }
    if (fakeDocker) {
      fakeDocker.close();
    }
    if (tempRoot) {
      rmSync(tempRoot, { recursive: true, force: true });
    }
  });

  test("updates service tile state as agent reports arrive", async ({ page }) => {
    const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: serviceName, exact: true }) });

    await page.goto(baseURL);
    await expect(page.locator(".sentence")).toHaveText("Waiting for the first scan");
    await page.getByRole("button", { name: "Sync repos" }).click();
    await expect(page.getByRole("heading", { name: "Production" })).toBeVisible();
    await expect(tile.locator(".stateWord")).toHaveText("No data");

    agentProcess = spawn(path.join(repoRoot, "gitops-dashboard"), ["-mode", "agent", "-config", agentConfigPath], {
      cwd: repoRoot
    });

    setContainerScenario("healthy");
    await waitForServiceHealth(baseURL, serviceName, "healthy");
    await page.reload();
    await expect(tile.locator(".stateWord")).toHaveText("Up");

    setContainerScenario("unhealthy");
    await waitForServiceHealth(baseURL, serviceName, "unhealthy");
    await page.reload();
    await expect(tile.locator(".stateWord")).toHaveText("Down");
  });
});

function createFixtureRepo(dir: string) {
  mkdirSync(path.join(dir, "prod"), { recursive: true });
  writeFileSync(path.join(dir, "prod", "compose.yaml"), [
    "services:",
    "  web:",
    "    image: example/web:v1.0.0",
    "    depends_on:",
    "      - db",
    "    environment:",
    "      APP_ENV: production",
    "    ports:",
    "      - \"8080:80\"",
    "    labels:",
    "      - \"traefik.http.routers.web.rule=Host('web.example.test')\"",
    "    volumes:",
    "      - \"/srv/web:/data\"",
    "    networks:",
    "      - frontend",
    ""
  ].join("\n"));
  writeFileSync(path.join(dir, "prod", "app.yaml"), [
    "apiVersion: apps/v1",
    "kind: Deployment",
    "metadata:",
    "  name: api",
    "  namespace: prod",
    "  labels:",
    "    app: api",
    "spec:",
    "  selector:",
    "    matchLabels:",
    "      app: api",
    "  template:",
    "    metadata:",
    "      labels:",
    "        app: api",
    "    spec:",
    "      containers:",
    "        - name: api",
    "          image: example/api:v1.0.0",
    "          ports:",
    "            - containerPort: 8080",
    "          envFrom:",
    "            - configMapRef:",
    "                name: api-config",
    "          readinessProbe:",
    "            httpGet:",
    "              path: /health",
    "              port: 8080",
    "          livenessProbe:",
    "            httpGet:",
    "              path: /health",
    "              port: 8080",
    "---",
    "apiVersion: v1",
    "kind: Service",
    "metadata:",
    "  name: api",
    "  namespace: prod",
    "spec:",
    "  selector:",
    "    app: api",
    "  ports:",
    "    - port: 80",
    "      targetPort: 8080",
    "---",
    "apiVersion: networking.k8s.io/v1",
    "kind: Ingress",
    "metadata:",
    "  name: api",
    "  namespace: prod",
    "spec:",
    "  rules:",
    "    - host: api.example.test",
    "      http:",
    "        paths:",
    "          - path: /",
    "            backend:",
    "              service:",
    "                name: api",
    "                port:",
    "                  number: 80",
    ""
  ].join("\n"));
  runGit(dir, "init", "-b", "main");
  runGit(dir, "config", "user.name", "Playwright");
  runGit(dir, "config", "user.email", "playwright@example.invalid");
  runGit(dir, "add", ".");
  runGit(dir, "commit", "-m", "fixture");
}

function runGit(cwd: string, ...args: string[]) {
  const result = spawnSync("git", args, { cwd, stdio: "pipe" });
  if (result.status === 0) {
    return;
  }
  const stderr = result.stderr.toString().trim();
  const stdout = result.stdout.toString().trim();
  const detail = stderr || stdout || result.error?.message || "";
  throw new Error(`git ${args.join(" ")} failed${detail ? `: ${detail}` : ""}`);
}

function freePort() {
  return new Promise<number>((resolve, reject) => {
    const listener = net.createServer();
    listener.on("error", reject);
    listener.listen(0, "127.0.0.1", () => {
      const address = listener.address();
      listener.close(() => {
        if (address && typeof address === "object") {
          resolve(address.port);
        } else {
          reject(new Error("unable to allocate a local port"));
        }
      });
    });
  });
}

function listen(server: Server, port: number) {
  return new Promise<void>((resolve, reject) => {
    server.on("error", reject);
    server.listen(port, "127.0.0.1", () => resolve());
  });
}

function createAgentFixtureRepo(dir: string, target: string, serviceName: string, image: string) {
  const composePath = path.join(dir, "docker_files", target, "prod");
  mkdirSync(composePath, { recursive: true });
  writeFileSync(path.join(composePath, "docker-compose.yaml"), [
    "services:",
    `  ${serviceName}:`,
    `    image: ${image}`,
    `    labels:`,
    `      - "com.docker.compose.project=${target}"`,
    ""
  ].join("\n"));
  runGit(dir, "init", "-b", "main");
  runGit(dir, "config", "user.name", "Playwright");
  runGit(dir, "config", "user.email", "playwright@example.invalid");
  runGit(dir, "add", ".");
  runGit(dir, "commit", "-m", "fixture");
}

async function waitForServiceHealth(url: string, serviceName: string, expectedHealth: "healthy" | "unhealthy" | "unknown") {
  const startedAt = Date.now();
  while (Date.now() - startedAt < 15_000) {
    try {
      const response = await fetch(`${url}/api/summary`);
      if (response.ok) {
        const summary = await response.json() as { services?: Array<{ name: string; health: string }> };
        const service = (summary.services ?? []).find((candidate) => candidate.name === serviceName);
        if (service?.health === expectedHealth) {
          return;
        }
      }
    } catch {
      // ignore transient failures
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`service ${serviceName} did not reach ${expectedHealth}`);
}

function createDynamicFakeDockerServer(readContainers: () => unknown[]) {
  return createServer((request, response) => {
    if (request.url?.startsWith("/containers/json")) {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify(readContainers()));
      return;
    }
    response.writeHead(404);
    response.end("not found");
  });
}

function createFakeDockerServer() {
  return createServer((request, response) => {
    if (request.url?.startsWith("/containers/json")) {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify([{
        Id: "container-web",
        Names: ["/fixture-web-1"],
        Image: "example/web:v1.0.0",
        ImageID: "sha256:container-web",
        State: "running",
        Status: "Up 5 minutes"
      }]));
      return;
    }
    response.writeHead(404);
    response.end("not found");
  });
}

function baseService(id: string, name: string, health: string) {
  return {
    id,
    name,
    repository: "fixture",
    sourceCommit: "abc123def456",
    runtime: "compose",
    kind: "Service",
    namespace: "",
    resourceName: name,
    sourcePath: "prod/compose.yaml",
    environment: "production",
    health,
    images: [`example/${name}:v1`],
    ports: [],
    dependencies: [],
    storage: [],
    exposure: [],
    monitorRoutes: [],
    configRefs: [],
    warnings: []
  };
}

function summaryShell(services: unknown[], uptime: unknown[], statuses: unknown[] = []) {
  const now = new Date().toISOString();
  return {
    repositories: [{
      name: "fixture",
      url: "file:///tmp/fixture",
      defaultRef: "main",
      lastCommit: "abc123",
      lastScanAt: now,
      status: "ok",
      error: ""
    }],
    scans: [{
      id: 1,
      repository: "fixture",
      status: "ok",
      commitSha: "abc123",
      startedAt: now,
      finishedAt: now,
      error: ""
    }],
    services,
    statuses,
    uptime,
    version: {
      version: "v1.2.3",
      commit: "abc123def4567890",
      buildDate: "2026-07-08T12:34:56Z"
    },
    generatedAt: now
  };
}

function summaryWithEveryHealthState() {
  const healthStates = ["healthy", "degraded", "unhealthy", "unknown", "error", "not_applicable"];
  return summaryShell(
    healthStates.map((health) => baseService(`svc-${health}`, `${health}-service`, health)),
    []
  );
}

function summaryWithImageVersionStates() {
  const serviceFor = (name: string, state: string, desired: string, observed?: string, repoDigests: string[] = []) => ({
    ...baseService(`svc-${name}`, name, "healthy"),
    images: [desired],
    desiredImages: [imageRef(desired)],
    imageVersionState: state,
    imageVersionChecks: [{
      desired: imageRef(desired),
      observed: observed ? {
        target: "runtime",
        runtime: "docker",
        reference: imageRef(observed),
        imageId: "sha256:observed",
        repoDigests: repoDigests.map(imageRef)
      } : undefined,
      state,
      message: state === "mismatched" ? "desired and observed image versions differ" : "image metadata state"
    }]
  });
  return summaryShell([
    serviceFor("matching-image", "matching", "example/matching-image:v1.0.0", "example/matching-image:v1.0.0"),
    serviceFor(
      "drifted-image",
      "mismatched",
      "example/drifted-image:v1.0.0",
      "example/drifted-image:v2.0.0",
      ["example/drifted-image@sha256:feedfacecafebeef00112233445566778899aabbccddeeff"]
    ),
    serviceFor("unknown-image", "unknown", "example/unknown-image:v1.0.0"),
    serviceFor("mutable-image", "mutable", "example/mutable-image:latest", "example/mutable-image:latest")
  ], []);
}

function summaryWithUptimeHistory() {
  const service = {
    ...baseService("svc-media", "media", "healthy"),
    exposure: ["media.example.test"],
    dependencies: ["db"]
  };
  const samples = ["healthy", "healthy", "unhealthy", "healthy", "healthy", "healthy"].map((health, index) => ({
    health,
    checkedAt: new Date(Date.now() - (6 - index) * 60_000).toISOString(),
    message: health === "unhealthy" ? "Exited (1) 5 minutes ago" : "Up 5 minutes"
  }));
  return summaryShell([service], [{
    serviceId: "svc-media",
    target: "local-docker",
    uptimePercent: 83.3,
    checkCount: 6,
    samples
  }]);
}

function imageRef(value: string) {
  const [repositoryAndTag, digest = ""] = value.split("@", 2);
  const lastSlash = repositoryAndTag.lastIndexOf("/");
  const lastColon = repositoryAndTag.lastIndexOf(":");
  const tag = lastColon > lastSlash ? repositoryAndTag.slice(lastColon + 1) : "";
  const name = tag ? repositoryAndTag.slice(0, lastColon) : repositoryAndTag;
  const firstSlash = name.indexOf("/");
  const first = firstSlash >= 0 ? name.slice(0, firstSlash) : "";
  const hasRegistry = first.includes(".") || first.includes(":") || first === "localhost";
  return {
    original: value,
    registry: hasRegistry ? first : "",
    repository: hasRegistry ? name.slice(firstSlash + 1) : name,
    tag,
    digest
  };
}

function summaryWithMixedTargetUptime() {
  const service = {
    ...baseService("svc-mixed", "mixed-monitor", "degraded"),
    exposure: ["mixed.example.test"]
  };
  const now = Date.now();
  const samplesFor = (healths: string[], target: string) => healths.map((health, index) => ({
    health,
    checkedAt: new Date(now - (healths.length - index) * 60_000).toISOString(),
    message: health === "healthy" ? `${target} ok` : `${target} failed`
  }));
  return summaryShell([service], [
    {
      serviceId: "svc-mixed",
      target: "route",
      uptimePercent: 100,
      checkCount: 3,
      samples: samplesFor(["healthy", "healthy", "healthy"], "route")
    },
    {
      serviceId: "svc-mixed",
      target: "docker",
      uptimePercent: 66.7,
      checkCount: 3,
      samples: samplesFor(["healthy", "error", "healthy"], "docker")
    }
  ]);
}

function summaryWithNotApplicableMonitor() {
  const now = Date.now();
  const service = {
    ...baseService("svc-routed", "routed-app", "healthy"),
    exposure: ["https://app.example.test", "http://10.10.10.20", "ssh://app.example.test:22"],
    monitorRoutes: ["https://app.example.test", "http://10.10.10.20"]
  };
  const goodTarget = "routes: https://app.example.test";
  const directTarget = "routes: http://10.10.10.20";
  const samples = ["healthy", "healthy", "healthy"].map((health, index) => ({
    health,
    checkedAt: new Date(now - (3 - index) * 60_000).toISOString(),
    message: "route ok"
  }));
  return summaryShell([service], [
    {
      serviceId: "svc-routed",
      target: goodTarget,
      uptimePercent: 100,
      checkCount: 3,
      samples
    }
  ], [
    {
      serviceId: "svc-routed",
      target: goodTarget,
      health: "healthy",
      message: "route ok",
      checkedAt: new Date(now).toISOString()
    },
    {
      serviceId: "svc-routed",
      target: directTarget,
      health: "not_applicable",
      message: "not applicable",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

function summaryWithLiteralRoutesMonitor() {
  const now = Date.now();
  const service = {
    ...baseService("svc-literal-routes", "literal-routes", "healthy"),
    exposure: ["ssh://literal.example.test:22"]
  };
  const samples = ["healthy", "healthy"].map((health, index) => ({
    health,
    checkedAt: new Date(now - (2 - index) * 60_000).toISOString(),
    message: "monitor ok"
  }));
  return summaryShell([service], [
    {
      serviceId: "svc-literal-routes",
      target: "routes",
      uptimePercent: 100,
      checkCount: 2,
      samples
    }
  ], [
    {
      serviceId: "svc-literal-routes",
      target: "routes",
      health: "healthy",
      message: "monitor ok",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

function summaryWithPostEnableAllRoutes() {
  const now = Date.now();
  const service = {
    ...baseService("svc-all-routes-enabled", "all-routes-enabled", "unknown"),
    exposure: ["https://app.example.test", "http://10.10.10.20", "ssh://app.example.test:22"],
    monitorRoutes: ["https://app.example.test", "http://10.10.10.20"]
  };
  return summaryShell([service], [], [
    {
      serviceId: "svc-all-routes-enabled",
      target: "routes",
      health: "unknown",
      message: "monitor enabled; waiting for next check",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

function summaryWithStaleOnlyRouteRows() {
  const now = Date.now();
  const service = {
    ...baseService("svc-stale-routes", "stale-routes", "unhealthy"),
    exposure: ["https://current.example.test", "ssh://stale.example.test:22"],
    monitorRoutes: ["https://current.example.test"]
  };
  const staleTarget = "routes: https://old.example.test";
  return summaryShell([service], [
    {
      serviceId: "svc-stale-routes",
      target: staleTarget,
      uptimePercent: 0,
      checkCount: 1,
      samples: [{
        health: "unhealthy",
        checkedAt: new Date(now).toISOString(),
        message: "stale route failed"
      }]
    }
  ], [
    {
      serviceId: "svc-stale-routes",
      target: staleTarget,
      health: "unhealthy",
      message: "stale route failed",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

function summaryWithCanonicalMonitorRoutes() {
  const now = Date.now();
  const service = {
    ...baseService("svc-canonical-routes", "canonical-routes", "healthy"),
    exposure: ["HTTPS://APP.EXAMPLE.TEST/", "https://app.example.test:443"],
    monitorRoutes: ["https://app.example.test"]
  };
  const target = "routes: https://app.example.test";
  return summaryShell([service], [
    {
      serviceId: "svc-canonical-routes",
      target,
      uptimePercent: 100,
      checkCount: 2,
      samples: [{
        health: "healthy",
        checkedAt: new Date(now - 60_000).toISOString(),
        message: "route ok"
      }]
    }
  ], [
    {
      serviceId: "svc-canonical-routes",
      target,
      health: "healthy",
      message: "route ok",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

function summaryWithSlashDistinctRoutes() {
  const now = Date.now();
  const service = {
    ...baseService("svc-slash-routes", "slash-routes", "healthy"),
    exposure: ["https://app.example.test/admin", "https://app.example.test/admin/"],
    monitorRoutes: ["https://app.example.test/admin", "https://app.example.test/admin/"]
  };
  const samples = (target: string) => [{
    health: "healthy",
    checkedAt: new Date(now - 60_000).toISOString(),
    message: `${target} ok`
  }];
  return summaryShell([service], [
    {
      serviceId: "svc-slash-routes",
      target: "routes: https://app.example.test/admin",
      uptimePercent: 100,
      checkCount: 1,
      samples: samples("admin")
    },
    {
      serviceId: "svc-slash-routes",
      target: "routes: https://app.example.test/admin/",
      uptimePercent: 100,
      checkCount: 1,
      samples: samples("admin slash")
    }
  ], [
    {
      serviceId: "svc-slash-routes",
      target: "routes: https://app.example.test/admin",
      health: "healthy",
      message: "admin ok",
      checkedAt: new Date(now).toISOString()
    },
    {
      serviceId: "svc-slash-routes",
      target: "routes: https://app.example.test/admin/",
      health: "healthy",
      message: "admin slash ok",
      checkedAt: new Date(now).toISOString()
    }
  ]);
}

type MonitorOverrideRequest = {
  serviceId: string;
  target: string;
  notApplicable: boolean;
};

function applyMonitorOverride(summary: ReturnType<typeof summaryWithNotApplicableMonitor>, override: MonitorOverrideRequest) {
  if (override.target === "routes" && override.notApplicable) {
    summary.statuses = summary.statuses.filter((status) => !status.target.startsWith("routes: "));
    summary.uptime = summary.uptime.filter((stat) => !stat.target.startsWith("routes: "));
  }
  const index = summary.statuses.findIndex((status) => (
    status.serviceId === override.serviceId && status.target === override.target
  ));
  if (!override.notApplicable) {
    if (index >= 0 && summary.statuses[index]?.health === "not_applicable") {
      summary.statuses.splice(index, 1);
    }
    return;
  }

  const status = {
    serviceId: override.serviceId,
    target: override.target,
    health: "not_applicable" as const,
    message: "not applicable",
    checkedAt: new Date().toISOString()
  };
  if (index >= 0) {
    summary.statuses[index] = status;
  } else {
    summary.statuses.push(status);
  }
}

async function waitForServer(url: string) {
  const startedAt = Date.now();
  while (Date.now() - startedAt < 10_000) {
    try {
      const response = await fetch(`${url}/healthz`);
      if (response.ok) {
        return;
      }
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
  throw new Error(`dashboard server did not become ready\n${serverLogs}`);
}
