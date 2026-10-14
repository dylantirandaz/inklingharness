"""One /tinker round: fine-tune a LoRA adapter on Think session traces.

Think embeds this file and runs it detached. It reads the traces Think wrote,
trains on every assistant turn in them, saves a resumable training state and
sampler weights, and writes the result JSON that Think reads back. Needs the
tinker and tinker-cookbook packages, and TINKER_API_KEY unless --dry-run.
"""

import argparse
import json
import sys
import traceback


def text(value):
    """Flatten Messages API content into plain text; images become a marker."""
    if value is None:
        return ""
    if isinstance(value, str):
        return value
    parts = []
    for block in value:
        if isinstance(block, dict) and block.get("type") == "text":
            parts.append(block.get("text", ""))
        elif isinstance(block, dict) and block.get("type") == "image":
            parts.append("[image omitted]")
    return "\n".join(parts)


def conversation(trace):
    """Convert one Messages API history to tinker-cookbook messages."""
    from tinker_cookbook.renderers.base import ToolCall

    history = []
    for message in trace["messages"]:
        role, content = message.get("role"), message.get("content")
        blocks = [{"type": "text", "text": content}] if isinstance(content, str) else content or []
        if role == "assistant":
            parts = [{"type": "thinking", "thinking": b["thinking"]} for b in blocks if b.get("type") == "thinking" and b.get("thinking")]
            parts += [{"type": "text", "text": b["text"]} for b in blocks if b.get("type") == "text" and b.get("text")]
            reply = {"role": "assistant", "content": parts}
            calls = [
                ToolCall(id=b.get("id"), function=ToolCall.FunctionBody(name=b["name"], arguments=json.dumps(b.get("input") or {}, ensure_ascii=False)))
                for b in blocks if b.get("type") == "tool_use"
            ]
            if calls:
                reply["tool_calls"] = calls
            history.append(reply)
        else:
            user = []
            for block in blocks:
                if block.get("type") == "tool_result":
                    history.append({"role": "tool", "tool_call_id": block.get("tool_use_id"), "content": text(block.get("content"))})
                elif block.get("type") in ("text", "image"):
                    user.append(text([block]))
            if user:
                history.append({"role": "user", "content": "\n".join(user)})
    return history


def tools(trace):
    return [{"name": t["name"], "description": t.get("description", ""), "parameters": t.get("input_schema") or {}} for t in trace.get("tools") or []]


def datums(traces, renderer, max_length):
    from tinker_cookbook.renderers import TrainOnWhat
    from tinker_cookbook.supervised.data import conversation_to_datum

    # Renderers that drop earlier reasoning (Qwen3, for example) see a
    # different prefix at each turn than a whole-session example shows, so
    # those get one example per assistant turn.
    whole = getattr(renderer, "has_extension_property", True)
    result = []
    for trace in traces:
        try:
            prefix = renderer.create_conversation_prefix_with_tools(tools(trace), trace.get("system", ""))
        except NotImplementedError:
            raise ValueError(f"{type(renderer).__name__} has no tool-calling format; choose a base model with tool calling, such as Qwen3, DeepSeek, Kimi or gpt-oss") from None
        history = conversation(trace)
        if whole:
            result.append(conversation_to_datum(prefix + history, renderer, max_length, TrainOnWhat.ALL_ASSISTANT_MESSAGES))
            continue
        for index, message in enumerate(history):
            if message["role"] == "assistant":
                result.append(conversation_to_datum(prefix + history[: index + 1], renderer, max_length, TrainOnWhat.LAST_ASSISTANT_MESSAGE))
    return result


def mean_loss(output, batch):
    total, weight = 0.0, 0.0
    for item, datum in zip(output.loss_fn_outputs, batch):
        logprobs = item["logprobs"].tolist()
        weights = datum.loss_fn_inputs["weights"].tolist()
        total += -sum(l * w for l, w in zip(logprobs, weights))
        weight += sum(weights)
    return total / weight if weight else 0.0


def train(arguments, traces):
    import tinker
    from tinker_cookbook import model_info, renderers

    service = tinker.ServiceClient()
    if arguments.resume:
        client = service.create_training_client_from_state_with_optimizer(arguments.resume)
    else:
        client = service.create_lora_training_client(base_model=arguments.base_model, rank=arguments.lora_rank)
    renderer = renderers.get_renderer(model_info.get_recommended_renderer_name(arguments.base_model), client.get_tokenizer())
    data = datums(traces, renderer, arguments.max_length)
    steps, loss = 0, 0.0
    for _ in range(arguments.epochs):
        for start in range(0, len(data), arguments.batch_size):
            batch = data[start:start + arguments.batch_size]
            backward = client.forward_backward(batch, "cross_entropy")
            step = client.optim_step(tinker.AdamParams(learning_rate=arguments.learning_rate))
            loss = mean_loss(backward.result(), batch)
            step.result()
            steps += 1
            print(json.dumps({"step": steps, "loss": loss}), flush=True)
    state = client.save_state(arguments.name).result().path
    sampler = client.save_weights_for_sampler(arguments.name).result().path
    return {"state_path": state, "sampler_path": sampler, "rows": len(data), "tokens": sum(d.model_input.length for d in data), "steps": steps, "loss": loss}


def dry_run(arguments, traces):
    from tinker_cookbook import model_info, renderers
    from tinker_cookbook.tokenizer_utils import get_tokenizer

    renderer = renderers.get_renderer(model_info.get_recommended_renderer_name(arguments.base_model), get_tokenizer(arguments.base_model))
    data = datums(traces, renderer, arguments.max_length)
    trained = sum(1 for d in data for w in d.loss_fn_inputs["weights"].tolist() if w > 0)
    return {"rows": len(data), "tokens": sum(d.model_input.length for d in data), "trained_tokens": trained}


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--traces", required=True)
    parser.add_argument("--base-model", required=True)
    parser.add_argument("--name", default="think")
    parser.add_argument("--out", required=True)
    parser.add_argument("--resume", help="Tinker training state to continue from")
    parser.add_argument("--learning-rate", type=float, default=1e-4)
    parser.add_argument("--lora-rank", type=int, default=32)
    parser.add_argument("--batch-size", type=int, default=4)
    parser.add_argument("--epochs", type=int, default=2)
    parser.add_argument("--max-length", type=int, default=32768)
    parser.add_argument("--dry-run", action="store_true")
    arguments = parser.parse_args(argv)
    with open(arguments.traces) as file:
        traces = [json.loads(line) for line in file if line.strip()]
    try:
        result = (dry_run if arguments.dry_run else train)(arguments, traces)
    except Exception as error:  # Think reads the error from the result file.
        traceback.print_exc()
        result = {"error": f"{type(error).__name__}: {error}"}
    with open(arguments.out, "w") as file:
        json.dump(result, file)
    print(json.dumps(result), flush=True)
    return 1 if "error" in result else 0


if __name__ == "__main__":
    sys.exit(main())
