import { useEffect, useMemo, useRef } from "react";

import { PulseStrip, ImageVersionList } from "./cards";
import { agentConnectionTone, agentConnectionWord, drawerSlots, statusWord } from "../constants";
import {
  accessTargets,
  agentConnection,
  containerTone,
  containerWord,
  environmentLabel,
  isPolicyBlocked,
  monitorOverrideKey,
  monitorTargetDetails,
  plural,
  relativeTime,
  runtimeLabel,
  sortContainers,
  targetBlockClass,
  targetDetailMeta
} from "../selectors";
import type { AgentInfo, Service, StatusResult, UptimeStat } from "../types";

export function ServiceDrawer({ busyMonitorOverride, onClose, onSetMonitorNotApplicable, service, statuses, uptime }: {
  busyMonitorOverride: string;
  onClose: () => void;
  onSetMonitorNotApplicable: (serviceId: string, target: string, notApplicable: boolean) => void;
  service: Service;
  statuses: StatusResult[];
  uptime: UptimeStat[];
}) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const routes = accessTargets(service);
  const commit = service.sourceCommit ? service.sourceCommit.slice(0, 7) : "";
  const targets = monitorTargetDetails(service, statuses, uptime);

  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <>
      <div aria-hidden="true" className="scrim" onClick={onClose} />
      <aside aria-labelledby="drawerTitle" aria-modal="true" className="drawer" role="dialog">
        <header className="drawerHead">
          <span aria-hidden="true" className={`dot big ${service.health}`} />
          <div className="drawerTitleBlock">
            <h2 id="drawerTitle">{service.name}</h2>
            <p className="drawerSub">
              {statusWord[service.health]} {"·"} {runtimeLabel(service.runtime)} {"·"} {environmentLabel(service.environment || "unassigned")}
            </p>
          </div>
          <button aria-label="Close details" className="drawerClose" onClick={onClose} ref={closeRef} type="button">
            <span aria-hidden="true">{"✕"}</span>
          </button>
        </header>

        <section className="drawerSection">
          <h3>Open</h3>
          {routes.length ? (
            <ul className="routeList">
              {routes.map((route) => (
                <li key={route.href}>
                  <a href={route.href} rel="noreferrer" target="_blank">
                    <span className="routeHost">{route.label}</span>
                    <span aria-hidden="true" className="doorArrow">{"↗"}</span>
                  </a>
                </li>
              ))}
            </ul>
          ) : (
            <p className="quiet">
              {service.runtime === "host" ? "No openable route was found for this host." : "No routes or DNS names were found in Git for this service."}
            </p>
          )}
        </section>

        {service.images.length ? (
          <section className="drawerSection">
            <h3>Image versions</h3>
            <ImageVersionList service={service} />
          </section>
        ) : null}

        <section className="drawerSection">
          <h3>Uptime</h3>
          {targets.length ? targets.map((target) => {
            const last = target.uptime?.samples[target.uptime.samples.length - 1] ?? null;
            const ignored = target.status?.health === "not_applicable";
            const blocked = isPolicyBlocked(target.status);
            const busy = busyMonitorOverride === monitorOverrideKey(service.id, target.target);
            return (
              <div className={targetBlockClass(target, ignored)} key={target.target}>
                <div className="targetHead">
                  <div className="targetTitle">
                    <strong>{target.label}</strong>
                    {target.kind !== "target" ? (
                      <span className="targetScope">
                        {target.kind === "service_routes" ? "Service override" : "Route"}
                      </span>
                    ) : null}
                  </div>
                  <span className="targetMeta">{targetDetailMeta(target)}</span>
                </div>
                {target.uptime ? <PulseStrip samples={target.uptime.samples} slots={drawerSlots} wide /> : null}
                {blocked ? (
                  <p className="targetNote">{target.status?.message ?? "blocked by policy"}</p>
                ) : ignored ? (
                  <p className="targetNote">
                    {statusWord.not_applicable}{target.status?.message ? ` — ${target.status.message}` : ""}
                  </p>
                ) : last ? (
                  <p className="targetNote">
                    {statusWord[last.health]}{last.message ? ` — ${last.message}` : ""} {"·"} {relativeTime(last.checkedAt)}
                  </p>
                ) : target.status ? (
                  <p className="targetNote">
                    {statusWord[target.status.health]}{target.status.message ? ` — ${target.status.message}` : ""} {"·"} {relativeTime(target.status.checkedAt)}
                  </p>
                ) : null}
                {blocked ? null : (
                  <button
                    className="targetToggle"
                    disabled={busy}
                    onClick={() => onSetMonitorNotApplicable(service.id, target.target, !ignored)}
                    type="button"
                  >
                    {busy ? "Saving..." : ignored ? "Enable monitor" : "Mark not applicable"}
                  </button>
                )}
              </div>
            );
          }) : (
            <p className="quiet">No checks yet. Check now to see live status.</p>
          )}
        </section>

        {service.dependencies.length ? (
          <section className="drawerSection">
            <h3>Depends on</h3>
            <div className="chips">
              {service.dependencies.map((dependency) => (
                <span className="chip" key={dependency}>{dependency}</span>
              ))}
            </div>
          </section>
        ) : null}

        <section className="drawerSection">
          <h3>{service.runtime === "host" ? "Configured from" : "Declared in Git"}</h3>
          <p className="provenance">
            {service.repository} {"·"} {service.sourcePath}{commit ? ` @ ${commit}` : ""}
          </p>
          {service.images.length ? <p className="provenance quiet">{service.images.join(", ")}</p> : null}
        </section>
      </aside>
    </>
  );
}
export function AgentDrawer({ agent, onClose }: { agent: AgentInfo; onClose: () => void }) {
  const closeRef = useRef<HTMLButtonElement>(null);
  const connection = agentConnection(agent);
  const wordTone = agentConnectionTone[connection];
  const containers = useMemo(() => sortContainers(agent.containers), [agent.containers]);

  useEffect(() => {
    closeRef.current?.focus();
    const onKey = (event: KeyboardEvent) => {
      if (event.key === "Escape") {
        onClose();
      }
    };
    document.addEventListener("keydown", onKey);
    return () => document.removeEventListener("keydown", onKey);
  }, [onClose]);

  return (
    <>
      <div aria-hidden="true" className="scrim" onClick={onClose} />
      <aside aria-labelledby="agentDrawerTitle" aria-modal="true" className="drawer" role="dialog">
        <header className="drawerHead">
          <span aria-hidden="true" className={`dot big ${wordTone}`} />
          <div className="drawerTitleBlock">
            <h2 id="agentDrawerTitle">{agent.target}</h2>
            <p className="drawerSub">
              {agentConnectionWord[connection]} {"·"} {agent.lastSeenAt ? `last report ${relativeTime(agent.lastSeenAt)}` : "no reports yet"}
            </p>
          </div>
          <button aria-label="Close details" className="drawerClose" onClick={onClose} ref={closeRef} type="button">
            <span aria-hidden="true">{"✕"}</span>
          </button>
        </header>

        <section className="drawerSection">
          <h3>Containers</h3>
          {containers.length ? (
            <ul className="containerList">
              {containers.map((container) => (
                <li key={container.id || container.name}>
                  <div className="containerRow">
                    <div className="containerName">
                      <strong>{container.name}</strong>
                      <span className="containerImage">{container.image}</span>
                    </div>
                    <span className={`stateWord ${containerTone(container)}`}>{containerWord(container)}</span>
                  </div>
                  <p className="containerRestarts">{container.restartCount} {plural(container.restartCount, "restart")}</p>
                </li>
              ))}
            </ul>
          ) : (
            <p className="quiet">No containers reported.</p>
          )}
        </section>
      </aside>
    </>
  );
}
