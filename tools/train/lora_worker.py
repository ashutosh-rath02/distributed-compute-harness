"""One round of LoRA fine-tuning on one device's share of the text.

The harness runs this (task type train.lora) with fixed arguments; it is
installed once per device in <tools-dir>/train next to a venv and the base
model. It reads its slice of the training text, optionally starts from the
adapter the last round produced, trains for a number of steps, and writes
the adapter and a small metrics file. Only the adapter (a few MB) travels
between devices, never the base model.
"""
import argparse
import json
import os
import time

import torch
from peft import LoraConfig, get_peft_model
from safetensors.torch import load_file, save_file
from transformers import AutoModelForCausalLM, AutoTokenizer


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--model", required=True)
    ap.add_argument("--data", required=True)
    ap.add_argument("--adapter-in", default="")
    ap.add_argument("--adapter-out", required=True)
    ap.add_argument("--metrics", required=True)
    ap.add_argument("--steps", type=int, default=20)
    ap.add_argument("--lr", type=float, default=2e-4)
    ap.add_argument("--threads", type=int, default=2)
    ap.add_argument("--block", type=int, default=128)
    ap.add_argument("--batch", type=int, default=4)
    ap.add_argument("--rank", type=int, default=8)
    ap.add_argument("--seed", type=int, default=1234)
    a = ap.parse_args()

    torch.set_num_threads(a.threads)
    tok = AutoTokenizer.from_pretrained(a.model)
    model = AutoModelForCausalLM.from_pretrained(a.model, torch_dtype=torch.float32)
    # The same seed on every device makes every device start from the same
    # adapter, which is what lets the average of their adapters mean something.
    torch.manual_seed(1234)
    cfg = LoraConfig(r=a.rank, lora_alpha=2 * a.rank, lora_dropout=0.0,
                     target_modules=["q_proj", "v_proj"], task_type="CAUSAL_LM")
    model = get_peft_model(model, cfg)
    if a.adapter_in:
        state = load_file(a.adapter_in)
        params = dict(model.named_parameters())
        for k, v in state.items():
            params[k].data.copy_(v)

    with open(a.data, encoding="utf-8", errors="replace") as f:
        ids = tok(f.read(), return_tensors="pt").input_ids[0]
    n_blocks = len(ids) // a.block
    if n_blocks < 1:
        raise SystemExit("not enough text: need at least %d tokens, have %d" % (a.block, len(ids)))
    x = torch.stack([ids[i * a.block:(i + 1) * a.block] for i in range(n_blocks)])

    trainable = [p for p in model.parameters() if p.requires_grad]
    opt = torch.optim.AdamW(trainable, lr=a.lr)
    g = torch.Generator().manual_seed(a.seed)
    model.train()
    losses = []
    t0 = time.time()
    for step in range(a.steps):
        idx = torch.randint(0, n_blocks, (min(a.batch, n_blocks),), generator=g)
        out = model(input_ids=x[idx], labels=x[idx])  # the model shifts the labels itself
        out.loss.backward()
        opt.step()
        opt.zero_grad()
        losses.append(out.loss.item())
        print("step %d/%d loss %.4f" % (step + 1, a.steps, losses[-1]), flush=True)
    secs = time.time() - t0

    state = {k: v.detach().clone().contiguous() for k, v in model.named_parameters() if v.requires_grad}
    save_file(state, a.adapter_out)
    k = max(1, min(5, len(losses)))
    with open(a.metrics, "w") as f:
        json.dump({"steps": a.steps, "secPerStep": secs / max(1, a.steps), "threads": a.threads,
                   "lossStart": sum(losses[:k]) / k, "lossEnd": sum(losses[-k:]) / k,
                   "tokens": int(a.steps * min(a.batch, n_blocks) * a.block)}, f)


if __name__ == "__main__":
    main()
