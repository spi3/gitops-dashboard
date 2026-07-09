import { agentConnectionTone, agentConnectionWord, imageVersionWord, statusWord, tileSlots } from "../constants";
import {
  accessTargets,
  agentCardTone,
  agentConnection,
  aggregateUptimeSamples,
  containerTally,
  formatDate,
  imageRefLabel,
  observedImageLabel,
  observedImageTitle,
  relativeTime,
  stripLabel,
  worstPercent
} from "../selectors";
import type { AgentInfo, Service, UptimeSample, UptimeStat } from "../types";

export function ServiceTile({ onOpen, service, uptime }: {
  onOpen: () => void;
  service: Service;
  uptime: UptimeStat[];
}) {
  const routes = accessTargets(service);
  const door = routes[0] ?? null;
  const samples = aggregateUptimeSamples(uptime);
  const percent = worstPercent(uptime);
  const lastSample = samples[samples.length - 1] ?? null;
  const imageState = service.imageVersionState ?? "unknown";

  return (
    <article
      aria-label={`${service.name}, ${statusWord[service.health]}`}
      className={`tile ${service.health}`}
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onOpen();
        }
      }}
      role="button"
      tabIndex={0}
    >
      <div className="tileTop">
        <span aria-hidden="true" className={`dot ${service.health}`} />
        <h3>{service.name}</h3>
        <span className={`stateWord ${service.health}`}>{statusWord[service.health]}</span>
      </div>
      {door ? (
        <span className="doorRow">
          <a
            className="door"
            href={door.href}
            onClick={(event) => event.stopPropagation()}
            rel="noreferrer"
            target="_blank"
          >
            {door.label}<span aria-hidden="true" className="doorArrow">{"↗"}</span>
          </a>
          {routes.length > 1 ? <span className="doorMore">+{routes.length - 1}</span> : null}
        </span>
      ) : (
        <span className="doorRow doorNone">no route in Git</span>
      )}
      {service.images.length ? (
        <span className={`imageBadge ${imageState}`}>{imageVersionWord[imageState]}</span>
      ) : null}
      <PulseStrip samples={samples} slots={tileSlots} />
      <div className="tileFoot">
        <span>{percent === null ? "no checks yet" : `${percent}% · 24h`}</span>
        <span>{lastSample ? relativeTime(lastSample.checkedAt) : ""}</span>
      </div>
    </article>
  );
}
export function PulseStrip({ samples, slots, wide }: { samples: UptimeSample[]; slots: number; wide?: boolean }) {
  const recent = samples.slice(-slots);
  const emptyCount = Math.max(0, slots - recent.length);
  return (
    <div aria-label={stripLabel(recent)} className={`pulseStrip ${wide ? "wide" : ""}`} role="img">
      {Array.from({ length: emptyCount }, (_, index) => (
        <span aria-hidden="true" className="tick empty" key={`empty-${index}`} />
      ))}
      {recent.map((sample, index) => (
        <span
          aria-hidden="true"
          className={`tick ${sample.health}`}
          key={`${sample.checkedAt}-${index}`}
          title={`${statusWord[sample.health]} · ${formatDate(sample.checkedAt)}${sample.message ? ` · ${sample.message}` : ""}`}
        />
      ))}
    </div>
  );
}
export function ImageVersionList({ service }: { service: Service }) {
  const checks = service.imageVersionChecks ?? [];
  if (checks.length === 0) {
    return (
      <div className="versionBlock unknown">
        <div className="versionHead">
          <strong>{service.images.join(", ")}</strong>
          <span className="imageBadge unknown">Image unknown</span>
        </div>
        <p className="targetNote">No runtime image metadata has been reported yet.</p>
      </div>
    );
  }
  return (
    <div className="versionList">
      {checks.map((check, index) => (
        <div className={`versionBlock ${check.state}`} key={`${check.desired.original || imageRefLabel(check.desired)}-${index}`}>
          <div className="versionHead">
            <strong>{imageRefLabel(check.desired)}</strong>
            <span className={`imageBadge ${check.state}`}>{imageVersionWord[check.state]}</span>
          </div>
          <dl className="versionFacts">
            <div>
              <dt>Desired</dt>
              <dd>{imageRefLabel(check.desired)}</dd>
            </div>
            {check.observed ? (
              <div>
                <dt>Observed</dt>
                <dd title={observedImageTitle(check.observed)}>{observedImageLabel(check.observed)}</dd>
              </div>
            ) : null}
            {check.observed?.target ? (
              <div>
                <dt>Target</dt>
                <dd>{check.observed.target}</dd>
              </div>
            ) : null}
          </dl>
          <p className="targetNote">{check.message}</p>
        </div>
      ))}
    </div>
  );
}
export function AgentCard({ agent, onOpen }: { agent: AgentInfo; onOpen: () => void }) {
  const connection = agentConnection(agent);
  const tone = agentCardTone(connection, agent.containers);
  const wordTone = agentConnectionTone[connection];

  return (
    <article
      aria-label={`${agent.target}, ${agentConnectionWord[connection]}`}
      className={`tile ${tone}`}
      onClick={onOpen}
      onKeyDown={(event) => {
        if (event.key === "Enter" || event.key === " ") {
          event.preventDefault();
          onOpen();
        }
      }}
      role="button"
      tabIndex={0}
    >
      <div className="tileTop">
        <span aria-hidden="true" className={`dot ${wordTone}`} />
        <h3>{agent.target}</h3>
        <span className={`stateWord ${wordTone}`}>{agentConnectionWord[connection]}</span>
      </div>
      <div className="tileFoot">
        <span>{agent.lastSeenAt ? `last report ${relativeTime(agent.lastSeenAt)}` : "no reports yet"}</span>
        <span>{containerTally(agent.containers)}</span>
      </div>
    </article>
  );
}
