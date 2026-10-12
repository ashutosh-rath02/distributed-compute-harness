#!/usr/bin/env bash
# Builds dist/install-training.sh: ONE file to paste into a macOS or Linux
# terminal. It sets up training on that device, in a private folder:
#   ~/.home-harness/tools/train/{venv,model,lora_worker.py}
# Nothing system-wide is touched; deleting that folder removes it all.
# The worker script is carried inside, so there is nothing else to copy.
# Run from the repo root:  bash scripts/build-training-kit.sh
set -euo pipefail
out=dist/install-training.sh
mkdir -p dist
cat > "$out" <<'HEAD'
#!/usr/bin/env bash
# Home Harness: set up federated LoRA training on this device.
# Installs PyTorch (CPU), transformers and peft into a private folder and
# downloads a 135M-parameter base model once (about 1 GB in all).
set -euo pipefail
T="$HOME/.home-harness/tools/train"
mkdir -p "$T"
cd "$T"

# PyTorch needs Python 3.9 to 3.12 (on an Intel Mac, PyTorch 2.2.2 is the last version).
PY=""
for c in python3.12 python3.11 python3.10 python3.9 python3; do
  if command -v "$c" >/dev/null 2>&1; then
    v=$("$c" -c 'import sys; print("%d.%d" % sys.version_info[:2])')
    case "$v" in 3.9|3.10|3.11|3.12) PY="$c"; break;; esac
  fi
done
if [ -z "$PY" ]; then
  echo "No Python 3.9 to 3.12 found. Install one (for example: brew install python@3.12) and run this again." >&2
  exit 1
fi
echo "Using $($PY --version)"

[ -d venv ] || "$PY" -m venv venv
PIP="./venv/bin/python -m pip install --default-timeout 180 --retries 10"
if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "x86_64" ]; then
  # The versions tested together with PyTorch 2.2.2: newer transformers
  # needs a newer PyTorch than an Intel Mac can have.
  $PIP "torch==2.2.2" "numpy<2" "transformers==4.44.2" "peft==0.12.0" "accelerate==0.34.2" "tokenizers<0.20" "huggingface_hub<0.26" safetensors
else
  $PIP torch --index-url https://download.pytorch.org/whl/cpu
  $PIP transformers peft safetensors numpy huggingface_hub
fi
./venv/bin/python - <<'PYEOF'
from huggingface_hub import snapshot_download
print("model", snapshot_download("HuggingFaceTB/SmolLM2-135M", local_dir="model", allow_patterns=["*.json", "*.safetensors", "*.txt"]))
PYEOF

cat > lora_worker.py <<'WORKER_EOF'
HEAD
cat tools/train/lora_worker.py >> "$out"
cat >> "$out" <<'TAIL'
WORKER_EOF

echo
echo "Training is set up in $T."
echo "Restart the Home Harness agent (or wait for it) and this device will offer train.lora."
TAIL
chmod +x "$out"
bash -n "$out"
echo "built $out ($(wc -c < "$out") bytes)"
