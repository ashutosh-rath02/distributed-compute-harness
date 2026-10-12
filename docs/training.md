# Training a model across your devices

`harnessctl train` fine-tunes a small language model on your devices together. Each device trains a small adapter (LoRA) on its own share of your text. The manager averages the adapters. The next round starts from the average. Only adapters (about 2 MB) travel between devices, never the model.

## Set up a device (once)

The device needs Python 3.9 to 3.12. Then, from the manager:

    harnessctl policy type train.setup on
    harnessctl do -target <node-id> train.setup

This installs PyTorch (CPU), the training libraries and a 135M-parameter base model into the agent's private tools folder (`train/`), about 1 GB. It is off until you turn it on in policy, it takes no parameters, and it installs only into that folder. Run it only on devices whose owners agree. Without the harness, `scripts/build-training-kit.sh` builds one file to paste into a Mac or Linux terminal instead.

## Train

    harnessctl train -data corpus.txt -rounds 3 -steps 8

Every ready device that offers `train.lora` takes part (or name them with `-nodes`). Each keeps the same share of the text in every round. From round 2 on, a slower device gets fewer steps so all finish together. A device uses a quarter of its cores by default, and a device at 10% battery or below takes no work. Each round's averaged adapter and each device's metrics are saved in `train-out/round-N/`.

## What to expect

Measured on a laptop (4 threads) and an Intel MacBook (2 threads): 2.8 to 5.6 seconds per step, about 40 to 120 seconds per round of 8 steps. Averaging LoRA adapters is an approximation (the A and B matrices are averaged separately), and it works best with few steps between averages. This shows distributed training works on home devices; it is not fast.

## Limits

- Phones can't train: PyTorch has no usable Android build.
- Devices must all start from the same adapter; the worker seeds it the same everywhere.
- Intel Macs are pinned to PyTorch 2.2.2 and older libraries; adapters from the two library sets have the same tensor names and shapes, so they average.
