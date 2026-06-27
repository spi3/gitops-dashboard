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
  await expectMetric(page, "unknown", "0");

  const repositoryPanel = panel(page, "Repositories");
  await expect(repositoryPanel.locator(".row").filter({ hasText: "fixture" })).toContainText("not scanned");
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(repositoryPanel.locator(".row").filter({ hasText: "fixture" })).toContainText("not scanned");

  await page.getByRole("button", { name: "Scan" }).click();

  await expect(repositoryPanel.locator(".row").filter({ hasText: "fixture" })).toContainText("ok");
  await expect(repositoryPanel.locator(".row").filter({ hasText: "fixture" })).not.toContainText("not scanned");
  await expectMetric(page, "unknown", "2");

  const servicesPanel = panel(page, "Services");
  const serviceGrid = servicesPanel.locator(".serviceGrid");
  const webCard = serviceGrid.locator("article.service").filter({ hasText: "web" });
  const apiCard = serviceGrid.locator("article.service").filter({ hasText: "api" });
  await expect(webCard).toBeVisible();
  await expect(webCard).toContainText("compose");
  await expect(webCard).toContainText("production");
  await expect(webCard).toContainText("prod/compose.yaml");
  await expect(webCard).toContainText("example/web:v1");
  await expect(webCard).toContainText("Config: APP_ENV");
  await expect(webCard).toContainText("missing healthcheck");
  await expect(apiCard).toBeVisible();
  await expect(apiCard).toContainText("kubernetes");

  const scanRow = panel(page, "Scan History").locator(".scanRow").filter({ hasText: "fixture" }).first();
  await expect(scanRow).toContainText("ok");
  await expect(scanRow).not.toContainText("not recorded");

  await servicesPanel.getByRole("combobox").selectOption("compose");
  await expect(serviceGrid.locator("article.service")).toHaveCount(1);
  await expect(webCard).toBeVisible();
  await servicesPanel.getByRole("combobox").selectOption("kubernetes");
  await expect(serviceGrid.locator("article.service")).toHaveCount(1);
  await expect(apiCard).toBeVisible();
  await expect(detailPanel(page)).toContainText("Deployment");
  await expect(detailPanel(page)).toContainText("prod");
  await expect(detailPanel(page)).toContainText("api");
  await servicesPanel.getByRole("combobox").selectOption("all");
  await expect(serviceGrid.locator("article.service")).toHaveCount(2);

  await page.locator("article.service").filter({ hasText: "api" }).click();
  const detail = detailPanel(page);
  await expect(detail.getByText("example/api:v1")).toBeVisible();
  await expect(detail).toContainText("8080");
  await expect(detail).toContainText("configMapRef/api-config");
  await expect(detail.getByText("No live runtime status has been recorded for this service.")).toBeVisible();

  await webCard.click();
  await expect(detail).toContainText("8080:80");
  await expect(detail).toContainText("db");
  await expect(detail).toContainText("/srv/web:/data");
  await expect(detail).toContainText("frontend");
  await expect(detail).toContainText("APP_ENV");

  await page.getByRole("button", { name: "Check Health" }).click();
  await expectMetric(page, "healthy", "1");
  await expectMetric(page, "unknown", "1");
  await expect(webCard.locator(".badge")).toHaveText("Healthy");
  await expect(detail.locator(".statusItem")).toContainText("local-docker");
  await expect(detail.locator(".statusItem")).toContainText("Up 5 minutes");
  await expect(detail.locator(".statusItem")).not.toContainText("not checked");

  await page.getByRole("button", { name: "Refresh" }).click();
  await expectMetric(page, "healthy", "1");
  await expect(detail.locator(".statusItem")).toContainText("local-docker");

  await page.route("**/api/summary", async (route) => {
    await route.fulfill({ status: 500, body: "summary unavailable" });
  });
  allowExpectedNetworkError = true;
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.locator("section.error")).toContainText("summary request failed: 500");
  await page.unroute("**/api/summary");
  await page.getByRole("button", { name: "Refresh" }).click();
  await expect(page.locator("section.error")).toBeHidden();
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
  for (const health of ["healthy", "degraded", "unhealthy", "unknown", "error"]) {
    await expectMetric(page, health, "1");
  }
  const serviceGrid = panel(page, "Services").locator(".serviceGrid");
  await expect(serviceGrid.locator(".badge.healthy")).toHaveText("Healthy");
  await expect(serviceGrid.locator(".badge.degraded")).toHaveText("Degraded");
  await expect(serviceGrid.locator(".badge.unhealthy")).toHaveText("Unhealthy");
  await expect(serviceGrid.locator(".badge.unknown")).toHaveText("Unknown");
  await expect(serviceGrid.locator(".badge.error")).toHaveText("Error");
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
    "spec:",
    "  template:",
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

function panel(page: Page, heading: string) {
  return page.locator("section.panel").filter({ has: page.getByRole("heading", { name: heading }) });
}

function detailPanel(page: Page) {
  return panel(page, "Service Detail");
}

async function expectMetric(page: Page, health: string, value: string) {
  const metric = page.locator(`.metric.${health}`);
  await expect(metric.locator("strong")).toHaveText(value);
}

function summaryWithEveryHealthState() {
  const now = new Date().toISOString();
  const healthStates = ["healthy", "degraded", "unhealthy", "unknown", "error"];
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
    services: healthStates.map((health, index) => ({
      id: `svc-${health}`,
      name: `${health}-service`,
      repository: "fixture",
      sourceCommit: "abc123",
      runtime: index % 2 === 0 ? "compose" : "kubernetes",
      kind: index % 2 === 0 ? "Service" : "Deployment",
      namespace: "prod",
      resourceName: `${health}-service`,
      sourcePath: "prod/app.yaml",
      environment: "production",
      health,
      images: [`example/${health}:v1`],
      ports: [],
      dependencies: [],
      storage: [],
      exposure: [],
      configRefs: [],
      warnings: []
    })),
    statuses: [],
    generatedAt: now
  };
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
