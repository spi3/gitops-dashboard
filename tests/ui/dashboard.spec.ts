import { expect, test, type Page } from "@playwright/test";
import { execFileSync, spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
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
  await expect(webTile.locator(".tick.healthy")).toHaveCount(1);
  await expect(webTile.getByText(/100% · 24h/)).toBeVisible();
  await expect(apiTile.locator(".stateWord")).toHaveText("No data");

  await webTile.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByRole("heading", { name: "web" })).toBeVisible();
  await expect(drawer.getByText(/Up · Compose · Production/)).toBeVisible();
  await expect(drawer.getByRole("link", { name: /web\.example\.test/ })).toHaveAttribute("href", "https://web.example.test");
  await expect(drawer.locator(".targetHead strong")).toHaveText("local-docker");
  await expect(drawer.locator(".targetHead span")).toHaveText(/100% · 1 check · 24h/);
  await expect(drawer.locator(".targetNote")).toContainText("Up 5 minutes");
  await expect(drawer.locator(".chip")).toHaveText("db");
  await expect(drawer.locator(".provenance").first()).toContainText("fixture · prod/compose.yaml @");
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();

  await expect(page.locator(".foot")).toContainText("last sync ok");

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
    ["error-service", "Check failed"]
  ];
  for (const [name, word] of expectations) {
    const tile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name, exact: true }) });
    await expect(tile.locator(".stateWord")).toHaveText(word);
  }

  await page.getByRole("button", { name: /Needs attention/ }).click();
  await expect(page.locator("article.tile")).toHaveCount(3);
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

function createFixtureRepo(dir: string) {
  mkdirSync(path.join(dir, "prod"), { recursive: true });
  writeFileSync(path.join(dir, "prod", "compose.yaml"), [
    "services:",
    "  web:",
    "    image: example/web:v1",
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
    "          image: example/api:v1",
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
  execFileSync("git", args, { cwd, stdio: "pipe" });
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

function createFakeDockerServer() {
  return createServer((request, response) => {
    if (request.url?.startsWith("/containers/json")) {
      response.writeHead(200, { "Content-Type": "application/json" });
      response.end(JSON.stringify([{
        Id: "container-web",
        Names: ["/fixture-web-1"],
        Image: "example/web:v1",
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
    configRefs: [],
    warnings: []
  };
}

function summaryShell(services: unknown[], uptime: unknown[]) {
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
    statuses: [],
    uptime,
    generatedAt: now
  };
}

function summaryWithEveryHealthState() {
  const healthStates = ["healthy", "degraded", "unhealthy", "unknown", "error"];
  return summaryShell(
    healthStates.map((health) => baseService(`svc-${health}`, `${health}-service`, health)),
    []
  );
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
