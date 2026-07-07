import { expect, test } from "@playwright/test";
import { execFileSync, spawn, type ChildProcessWithoutNullStreams } from "node:child_process";
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { createServer, type Server } from "node:http";
import { tmpdir } from "node:os";
import path from "node:path";
import net from "node:net";
import { fileURLToPath } from "node:url";

const repoRoot = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");

// Fixture docker container returned by the fake docker HTTP server that the
// spawned agent process polls. Assertions below derive their expectations
// from this fixture rather than hardcoding them twice.
const fixtureContainers = [
  {
    Id: "container-web",
    Names: ["/fixture-web-1"],
    Image: "example/web:v1",
    State: "running",
    Status: "Up 5 minutes"
  }
];
const expectedRunningCount = fixtureContainers.filter((container) => container.State === "running").length;
const expectedContainerTally = `${expectedRunningCount} of ${fixtureContainers.length} running`;

const agentToken = "e2e-agent-token";
const reportingTarget = "rpi-agent";
const neverReportingTarget = "ghost-agent";

let tempRoot = "";
let baseURL = "";
let server: ChildProcessWithoutNullStreams | null = null;
let agentProcess: ChildProcessWithoutNullStreams | null = null;
let fakeDocker: Server | null = null;
let serverLogs = "";
let agentLogs = "";

test.beforeAll(async () => {
  tempRoot = mkdtempSync(path.join(tmpdir(), "gitops-dashboard-agents-ui-"));
  const fixtureRepo = path.join(tempRoot, "fixture");
  const dataDir = path.join(tempRoot, "data");
  const serverConfigPath = path.join(tempRoot, "server.yaml");
  const agentConfigPath = path.join(tempRoot, "agent.yaml");

  createFixtureRepo(fixtureRepo);
  mkdirSync(dataDir, { recursive: true });

  const serverPort = await freePort();
  const dockerPort = await freePort();
  fakeDocker = createFakeDockerServer();
  await listen(fakeDocker, dockerPort);

  baseURL = `http://127.0.0.1:${serverPort}`;

  writeFileSync(serverConfigPath, [
    "server:",
    `  listen: "127.0.0.1:${serverPort}"`,
    `  dataDir: "${dataDir}"`,
    `  repoCacheDir: "${path.join(dataDir, "repos")}"`,
    "auth:",
    "  mode: dev-no-auth",
    "  agent:",
    "    tokens:",
    `      - "${agentToken}"`,
    "monitoring:",
    "  defaultInterval: 30s",
    "repositories:",
    "  - name: fixture",
    `    url: "file://${fixtureRepo}"`,
    "    defaultRef: main",
    "runtime:",
    "  docker:",
    `    - name: "${reportingTarget}"`,
    "      kind: agent",
    `    - name: "${neverReportingTarget}"`,
    "      kind: agent",
    "  kubernetes: []",
    ""
  ].join("\n"));

  server = spawn(path.join(repoRoot, "gitops-dashboard"), ["-config", serverConfigPath], {
    cwd: repoRoot
  });
  server.stdout.on("data", (chunk) => {
    serverLogs += chunk.toString();
  });
  server.stderr.on("data", (chunk) => {
    serverLogs += chunk.toString();
  });
  await waitForServer(baseURL, () => serverLogs);

  writeFileSync(agentConfigPath, [
    "agent:",
    `  serverUrl: "ws://127.0.0.1:${serverPort}/api/agents/connect"`,
    `  target: "${reportingTarget}"`,
    `  token: "${agentToken}"`,
    "  interval: \"1s\"",
    "  docker:",
    `    host: "http://127.0.0.1:${dockerPort}"`,
    ""
  ].join("\n"));

  agentProcess = spawn(path.join(repoRoot, "gitops-dashboard"), ["-mode", "agent", "-config", agentConfigPath], {
    cwd: repoRoot
  });
  agentProcess.stdout.on("data", (chunk) => {
    agentLogs += chunk.toString();
  });
  agentProcess.stderr.on("data", (chunk) => {
    agentLogs += chunk.toString();
  });

  await waitForAgentReport(baseURL, reportingTarget, () => `${serverLogs}\n---agent---\n${agentLogs}`);
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

test("agents tab shows connected and never-connected agents without losing the services tab", async ({ page }) => {
  const pageErrors: string[] = [];
  page.on("pageerror", (error) => {
    pageErrors.push(error.message);
  });

  await page.goto(baseURL);
  await expect(page.getByRole("heading", { name: "GitOps Dashboard" })).toBeVisible();

  // Populate the Services tab first, so we can prove switching tabs and back
  // doesn't discard its state.
  await page.getByRole("button", { name: "Sync repos" }).click();
  await expect(page.getByRole("heading", { name: "Production" })).toBeVisible();
  const webTile = page.locator("article.tile").filter({ has: page.getByRole("heading", { name: "web", exact: true }) });
  await expect(webTile).toBeVisible();

  const servicesTab = page.getByRole("tab", { name: "Services" });
  const agentsTab = page.getByRole("tab", { name: /^Agents/ });
  await expect(servicesTab).toHaveAttribute("aria-selected", "true");

  await agentsTab.click();
  await expect(agentsTab).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/#\/agents$/);
  await expect(page.getByRole("tabpanel")).toHaveAttribute("id", "agentsPanel");

  const reportingCard = page.locator("article.tile").filter({
    has: page.getByRole("heading", { name: reportingTarget, exact: true })
  });
  const neverCard = page.locator("article.tile").filter({
    has: page.getByRole("heading", { name: neverReportingTarget, exact: true })
  });

  await expect(reportingCard).toBeVisible();
  await expect(neverCard).toBeVisible();

  await expect(reportingCard.locator(".stateWord")).toHaveText("Connected");
  await expect(reportingCard).toContainText(expectedContainerTally);
  await expect(reportingCard).toContainText(/last report/);

  await expect(neverCard.locator(".stateWord")).toHaveText("Never connected");
  await expect(neverCard).toContainText("no reports yet");
  await expect(neverCard).toContainText("no containers reported");

  // Opening the reporting agent's card shows a drawer listing its containers.
  await reportingCard.click();
  const drawer = page.getByRole("dialog");
  await expect(drawer.getByRole("heading", { name: reportingTarget })).toBeVisible();
  await expect(drawer.locator(".containerName strong")).toHaveText(fixtureContainers[0].Names[0]);
  await expect(drawer.locator(".containerImage")).toHaveText(fixtureContainers[0].Image);
  await expect(drawer.locator(".containerList li")).toHaveCount(fixtureContainers.length);
  await page.keyboard.press("Escape");
  await expect(drawer).toBeHidden();

  // Switching back to Services must not have lost what we synced earlier.
  await servicesTab.click();
  await expect(servicesTab).toHaveAttribute("aria-selected", "true");
  await expect(page).toHaveURL(/#\/$/);
  await expect(page.getByRole("heading", { name: "Production" })).toBeVisible();
  await expect(webTile).toBeVisible();

  expect(pageErrors).toEqual([]);
});

test("the #/agents deep link opens directly on the agents tab from a fresh page load", async ({ page }) => {
  await page.goto(`${baseURL}/#/agents`);

  const agentsTab = page.getByRole("tab", { name: /^Agents/ });
  const servicesTab = page.getByRole("tab", { name: "Services" });
  await expect(agentsTab).toHaveAttribute("aria-selected", "true");
  await expect(servicesTab).toHaveAttribute("aria-selected", "false");

  await expect(page.locator("#agentsPanel")).toBeVisible();
  await expect(page.locator("#servicesPanel")).toHaveCount(0);

  const reportingCard = page.locator("article.tile").filter({
    has: page.getByRole("heading", { name: reportingTarget, exact: true })
  });
  await expect(reportingCard.locator(".stateWord")).toHaveText("Connected");
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
      response.end(JSON.stringify(fixtureContainers));
      return;
    }
    response.writeHead(404);
    response.end("not found");
  });
}

async function waitForServer(url: string, logs: () => string) {
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
  throw new Error(`dashboard server did not become ready\n${logs()}`);
}

async function waitForAgentReport(url: string, target: string, logs: () => string) {
  const startedAt = Date.now();
  while (Date.now() - startedAt < 15_000) {
    try {
      const response = await fetch(`${url}/api/summary`);
      if (response.ok) {
        const summary = (await response.json()) as { agents?: Array<{ target: string; lastSeenAt: string }> };
        const agent = (summary.agents ?? []).find((candidate) => candidate.target === target);
        if (agent && agent.lastSeenAt !== "") {
          return;
        }
      }
    } catch {
      // server may not be ready to answer yet; keep polling.
    }
    await new Promise((resolve) => setTimeout(resolve, 100));
  }
  throw new Error(`agent ${target} never reported to the server\n${logs()}`);
}
