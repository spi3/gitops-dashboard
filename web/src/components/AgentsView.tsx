import { AgentCard } from "./cards";
import { plural, relativeTime } from "../selectors";
import type { AgentInfo, Tone } from "../types";

type AgentsViewProps = {
  agentOverall: { tone: Tone; sentence: string };
  agents: AgentInfo[];
  latestAgentReport: string;
  onOpenAgent: (target: string) => void;
};

export function AgentsView({ agentOverall, agents, latestAgentReport, onOpenAgent }: AgentsViewProps) {
  return (
    <div aria-labelledby="tab-agents" className="tabPanel" id="agentsPanel" role="tabpanel">
      <section aria-live="polite" className="hero">
        <span aria-hidden="true" className={`beacon ${agentOverall.tone}`} />
        <div>
          <p className="sentence">{agentOverall.sentence}</p>
          <p className="heroMeta">
            {agents.length} {plural(agents.length, "agent")}
            {latestAgentReport ? ` · last report ${relativeTime(latestAgentReport)}` : ""}
          </p>
        </div>
      </section>

      <main>
        {agents.length === 0 ? (
          <div className="emptyState">
            <p className="emptyLead">No agents yet</p>
            <p>Agents connect once you run this binary with mode: agent pointed at this server.</p>
          </div>
        ) : (
          <div className="tiles">
            {agents.map((agent) => (
              <AgentCard agent={agent} key={agent.target} onOpen={() => onOpenAgent(agent.target)} />
            ))}
          </div>
        )}
      </main>
    </div>
  );
}
