import type { ExtensionAPI } from "@oh-my-pi/pi-coding-agent";

// The common boundary, not fail-open extension hooks, enforces every request.
export default function (pi: ExtensionAPI): void {
  const baseUrl = process.env.HARBOR_MESSAGES_BASE_URL;
  const apiKey = process.env.HARBOR_WIRE_TOKEN;
  if (!baseUrl || !apiKey) {
    throw new Error("OMP requires the Harbor Messages recorder endpoint and token.");
  }
  pi.registerProvider("inkling-eval", {
    baseUrl,
    api: "anthropic-messages",
    apiKey,
    models: [{
      id: "thinkingmachines/inkling-small",
      name: "Inkling Small",
      reasoning: true,
      input: ["text"],
      cost: { input: 0.45, output: 1.2, cacheRead: 0.1, cacheWrite: 0 },
      contextWindow: 524288,
      maxTokens: 16384,
    }],
  });
  pi.on("before_subagent_spawn", () => ({
    model: "inkling-eval/thinkingmachines/inkling-small:high",
  }));
}
