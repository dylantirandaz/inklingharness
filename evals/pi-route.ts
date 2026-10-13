import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// The recorder normalizes every Messages request, including native compaction.
// This extension only selects the existing native transport and model metadata.
export default function (pi: ExtensionAPI): void {
  const baseUrl = process.env.HARBOR_MESSAGES_BASE_URL;
  const apiKey = process.env.HARBOR_WIRE_TOKEN;
  const id = process.env.HARBOR_MODEL_ID;
  const contextWindow = Number(process.env.HARBOR_MODEL_CONTEXT);
  const maxTokens = Number(process.env.HARBOR_MODEL_MAX_TOKENS);
  const [input, output, cacheRead] = (process.env.HARBOR_MODEL_PRICES ?? "").split(",").map(Number);
  if (!baseUrl || !apiKey) {
    throw new Error("Pi requires the Harbor Messages recorder endpoint and token.");
  }
  if (!id || !(contextWindow > 0) || !(maxTokens > 0) || ![input, output, cacheRead].every((price) => price >= 0)) {
    throw new Error("Pi requires the Harbor model profile.");
  }
  pi.registerProvider("inkling-eval", {
    baseUrl,
    api: "anthropic-messages",
    apiKey,
    models: [{
      id,
      name: id,
      reasoning: true,
      input: ["text"],
      cost: { input, output, cacheRead, cacheWrite: 0 },
      contextWindow,
      maxTokens,
    }],
  });
}
