"""Fine-tune a Tinker model on conversations exported by evals.tinker_dataset.

This is rejection-sampling fine-tuning: export the passing trials of earlier
benchmark runs, train a LoRA adapter on every assistant turn in them, and
evaluate loss on tasks held out of training. Each row's recorded tool list is
rendered with the model's own tool-declaration format, so the model learns the
harness's tools as it saw them.

  python -m evals.tinker_train --train out/tinker/train.jsonl --eval out/tinker/eval.jsonl --dry-run
  TINKER_API_KEY=... python -m evals.tinker_train --train ... --eval ... --log-path out/tinker/run-1

--dry-run renders every row locally and prints token counts and the training
cost estimate without contacting Tinker. Needs tinker and tinker-cookbook.
"""

import argparse
import asyncio
import json
import os
from pathlib import Path
import sys

import chz
import datasets
import tinker

from tinker_cookbook import cli_utils, model_info
from tinker_cookbook.renderers import Renderer, TrainOnWhat
from tinker_cookbook.supervised import train
from tinker_cookbook.supervised.data import SupervisedDatasetFromHFDataset, conversation_to_datum
from tinker_cookbook.supervised.types import ChatDatasetBuilder, ChatDatasetBuilderCommonConfig, SupervisedDataset

DEFAULT_MODEL = "thinkingmachines/Inkling-Small"
# Per-million-token training price on Tinker (Inkling-Small, 64K context, with
# the limited-time discount listed on the Models & Pricing page in Oct 2026).
TRAIN_PRICE = {"thinkingmachines/Inkling-Small": 1.73, "thinkingmachines/Inkling": 5.61}


def conversation(row: dict[str, object], renderer: Renderer) -> list[dict[str, object]]:
    """Put the row's system prompt and tools in the renderer's own prefix."""
    messages = row["messages"]
    system = "\n\n".join(message["content"] for message in messages if message["role"] == "system")
    prefix = renderer.create_conversation_prefix_with_tools(row.get("tools") or [], system)
    return prefix + [message for message in messages if message["role"] != "system"]


def read_rows(path: str) -> list[dict[str, object]]:
    with open(path) as file:
        # The HF dataset needs one schema; keep the payload as JSON text.
        return [{"row": line.strip()} for line in file if line.strip()]


@chz.chz
class HarnessRunsBuilder(ChatDatasetBuilder):
    """Training rows from tinker_dataset, with an optional held-out file."""

    train_path: str
    eval_path: str | None = None
    # Every eval scores all held-out rows at the training price, so keep it small.
    eval_rows: int = 32
    shuffle_seed: int = 0

    def __call__(self) -> tuple[SupervisedDataset, SupervisedDataset | None]:
        renderer = self.renderer
        max_length = self.common_config.max_length
        train_on_what = TrainOnWhat(self.common_config.train_on_what or TrainOnWhat.ALL_ASSISTANT_MESSAGES)

        def to_datum(item: dict[str, str]) -> tinker.Datum:
            return conversation_to_datum(conversation(json.loads(item["row"]), renderer), renderer, max_length, train_on_what)

        train_rows = datasets.Dataset.from_list(read_rows(self.train_path)).shuffle(seed=self.shuffle_seed)
        train_set = SupervisedDatasetFromHFDataset(train_rows, batch_size=self.common_config.batch_size, map_fn=to_datum)
        eval_set = None
        if self.eval_path:
            eval_rows = datasets.Dataset.from_list(read_rows(self.eval_path)).shuffle(seed=self.shuffle_seed)
            eval_rows = eval_rows.select(range(min(self.eval_rows, len(eval_rows))))
            eval_set = SupervisedDatasetFromHFDataset(eval_rows, batch_size=min(len(eval_rows), self.common_config.batch_size), map_fn=to_datum)
        return train_set, eval_set


def _eval_tokens(builder: HarnessRunsBuilder) -> int:
    if not builder.eval_path:
        return 0
    _, eval_set = builder()
    return sum(datum.model_input.length for index in range(len(eval_set)) for datum in eval_set.get_batch(index))


def dry_run(builder: HarnessRunsBuilder, model: str, epochs: int, eval_every: int) -> dict[str, object]:
    renderer = builder.renderer
    lengths, trained, truncated = [], 0, 0
    for item in read_rows(builder.train_path):
        row = json.loads(item["row"])
        full = conversation_to_datum(conversation(row, renderer), renderer, None).model_input.length
        datum = conversation_to_datum(conversation(row, renderer), renderer, builder.common_config.max_length)
        lengths.append(datum.model_input.length)
        trained += sum(1 for weight in datum.loss_fn_inputs["weights"].tolist() if weight > 0)
        truncated += full > datum.model_input.length
    lengths.sort()
    tokens = sum(lengths)
    price = TRAIN_PRICE.get(model)
    steps = -(-len(lengths) // builder.common_config.batch_size) * epochs
    evals = steps // eval_every if eval_every and builder.eval_path else 0
    eval_tokens = _eval_tokens(builder)
    return {
        "model": model,
        "rows": len(lengths),
        "truncated_rows": truncated,
        "tokens_per_epoch": tokens,
        "trained_tokens_per_epoch": trained,
        "tokens_p50": lengths[len(lengths) // 2] if lengths else 0,
        "tokens_max": lengths[-1] if lengths else 0,
        "steps": steps,
        "eval_tokens_each": eval_tokens,
        "evals": evals,
        "estimated_train_cost_usd": round(tokens * epochs * price / 1e6, 2) if price else None,
        "estimated_eval_cost_usd": round(eval_tokens * evals * price / 1e6, 2) if price else None,
    }


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    parser.add_argument("--train", required=True, help="Training JSONL from evals.tinker_dataset")
    parser.add_argument("--eval", help="Held-out JSONL from evals.tinker_dataset --eval-out")
    parser.add_argument("--model", default=DEFAULT_MODEL)
    parser.add_argument("--log-path", default="out/tinker/sft")
    parser.add_argument("--max-length", type=int, default=65536, help="Rows longer than this are truncated")
    parser.add_argument("--batch-size", type=int, default=16, help="Conversations per optimizer step")
    parser.add_argument("--learning-rate", type=float, default=1e-4)
    parser.add_argument("--lora-rank", type=int, default=32)
    parser.add_argument("--epochs", type=int, default=1)
    parser.add_argument("--save-every", type=int, default=10)
    parser.add_argument("--eval-every", type=int, default=5)
    parser.add_argument("--eval-rows", type=int, default=32, help="Held-out rows scored at each eval")
    parser.add_argument("--max-steps", type=int)
    parser.add_argument("--dry-run", action="store_true", help="Count tokens and estimate cost; do not train")
    arguments = parser.parse_args(argv)

    renderer_name = model_info.get_recommended_renderer_name(arguments.model)
    common = ChatDatasetBuilderCommonConfig(
        model_name_for_tokenizer=arguments.model,
        renderer_name=renderer_name,
        max_length=arguments.max_length,
        batch_size=arguments.batch_size,
        train_on_what=TrainOnWhat.ALL_ASSISTANT_MESSAGES,
    )
    builder = HarnessRunsBuilder(common_config=common, train_path=arguments.train, eval_path=arguments.eval, eval_rows=arguments.eval_rows)
    if arguments.dry_run:
        print(json.dumps(dry_run(builder, arguments.model, arguments.epochs, arguments.eval_every), indent=2))
        return 0
    if not os.environ.get("TINKER_API_KEY"):
        print("TINKER_API_KEY is not set; use --dry-run to check the data without training.", file=sys.stderr)
        return 2
    config = train.Config(
        log_path=str(Path(arguments.log_path)),
        model_name=arguments.model,
        recipe_name="inklingharness_rft",
        renderer_name=renderer_name,
        dataset_builder=builder,
        learning_rate=arguments.learning_rate,
        lr_schedule="linear",
        num_epochs=arguments.epochs,
        lora_rank=arguments.lora_rank,
        save_every=arguments.save_every,
        eval_every=arguments.eval_every if arguments.eval else 0,
        max_steps=arguments.max_steps,
    )
    cli_utils.check_log_dir(config.log_path, behavior_if_exists="ask")
    asyncio.run(train.main(config))
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
