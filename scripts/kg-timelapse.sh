#!/usr/bin/env bash
# kg-timelapse — replay the demo corpus session by session and render the
# knowledge graph growing, one frame per session. Uses `graymatter kg render`
# as the only GrayMatter-side primitive (the playbook's contract: the script
# adds no UI of its own).
#
# Pipeline per frame:
#   1. seed a fresh store with the first N sessions of the corpus
#   2. run consolidation so the KG extractor feeds the graph (deterministic,
#      regex-based when no LLM is configured)
#   3. `graymatter kg render --out frames/frame-NN.dot`
#   4. rasterise the frame with Graphviz `dot` when available
#
# Assembly into the final GIF uses ImageMagick (`magick`) or ffmpeg, whichever
# is on PATH. If neither is installed, the frames stay in frames/ with exact
# instructions to assemble them — the script never fails on a missing
# assembler, it degrades with instructions (same philosophy as the hooks).
#
# Everything is deterministic: the corpus is inline, sessions replay in order,
# the frame count and content depend only on this script. Re-running produces
# byte-identical dot output for each frame.
#
# Usage:
#   scripts/kg-timelapse.sh [output-dir]        # default: kg-timelapse-out
#   scripts/kg-timelapse.sh --binary /path/to/graymatter   # test an uninstalled build
set -euo pipefail

BINARY="${GRAYMATTER_BINARY:-graymatter}"
OUT="kg-timelapse-out"
if [[ "${1:-}" == "--binary" ]]; then BINARY="${2:?--binary needs a path}"; shift 2; fi
if [[ "${1:-}" != "" ]]; then OUT="$1"; fi

command -v "$BINARY" >/dev/null 2>&1 || { echo "graymatter binary '$BINARY' not found on PATH (or pass --binary)" >&2; exit 1; }

FRAMES="$OUT/frames"
mkdir -p "$FRAMES"

# ---------------------------------------------------------------------------
# The corpus: a working week, grouped into "sessions" the way real sessions
# deposit facts. Ordered; frame N replays sessions 1..N. Names use the
# multi-word capitalized and org-suffix shapes the deterministic extractor
# recognises (single ambiguous words are deliberately not extracted), and
# several facts co-mention two entities so co-mention edges appear as the
# graph grows.
# ---------------------------------------------------------------------------
SESSIONS=(
  # session 1 — the deal opens
  "sales-closer|Maria Rodriguez opened a pilot conversation with Acme Corp about expanding beyond 40 seats."
  # session 2
  "sales-closer|Acme Corp's renewal is worth 84k ARR; Maria Rodriguez will send the proposal by Friday."
  # session 3
  "support-lead|Ticket 4417 from Acme Corp was escalated to engineering and Sofia Herrera picked it up."
  # session 4
  "infra-bot|Priya Sharma deployed the Postgres 16 upgrade for Acme Corp during the Tuesday window."
  # session 5
  "sales-closer|Kenji Watanabe joined the Acme Corp security review; Maria Rodriguez sent the SOC 2 report."
  # session 6
  "support-lead|Sofia Herrera closed ticket 4417 and filed the search slowness follow-up with Priya Sharma."
  # session 7 — shared conventions carry no entities; the graph shows only what is real
  "__shared__|Project convention: every timestamp is stored as UTC ISO-8601."
)

echo "==> rendering ${#SESSIONS[@]} sessions into $FRAMES"
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT

STORE="$WORK/store"
mkdir -p "$STORE"
touch "$STORE/kg.auto"   # KG auto-population sentinel (same as init --kg)

FRAME=0
for ENTRY in "${SESSIONS[@]}"; do
  AGENT="${ENTRY%%|*}"
  FACT="${ENTRY#*|}"
  FRAME=$((FRAME + 1))
  NAME=$(printf "frame-%03d" "$FRAME")

  "$BINARY" --dir "$STORE" remember "$AGENT" "$FACT" --quiet
  "$BINARY" --dir "$STORE" consolidate "$AGENT" --quiet || true

  if "$BINARY" --dir "$STORE" kg render --out "$FRAMES/$NAME.dot" --quiet 2>/dev/null \
     || "$BINARY" --dir "$STORE" kg render --out "$FRAMES/$NAME.dot"; then
    echo "    $NAME  (graph state written)"
  else
    # The graph may be empty in the first sessions; render an empty frame so
    # the timelapse still shows the build-up from zero.
    printf 'digraph graymatter { label="session %d — graph empty"; }\n' "$FRAME" > "$FRAMES/$NAME.dot"
    echo "    $NAME  (empty graph placeholder)"
  fi
done

echo "==> dot frames written to $FRAMES"

# --- rasterise + assemble (best-effort, with instructions on failure) -------
RASTER=0
if command -v dot >/dev/null 2>&1; then
  for f in "$FRAMES"/*.dot; do
    dot -Tpng -Gdpi=220 "$f" -o "${f%.dot}.png"
  done
  RASTER=1
  echo "==> PNGs rendered with graphviz"
else
  echo "!!  graphviz (dot) not found: skipping rasterisation"
  echo "    install it, then:  for f in $FRAMES/*.dot; do dot -Tpng -Gdpi=220 \$f -o \${f%.dot}.png; done"
fi

if [[ "$RASTER" == "1" ]]; then
  GIF="$OUT/kg-timelapse.gif"
  if command -v magick >/dev/null 2>&1; then
    # ImageMagick 7
    magick -delay 60 -loop 0 "$FRAMES"/frame-*.png "$GIF"
    echo "==> GIF assembled: $GIF"
  elif command -v convert >/dev/null 2>&1; then
    # ImageMagick 6 (Debian bookworm and friends ship this as convert)
    convert -delay 60 -loop 0 "$FRAMES"/frame-*.png "$GIF"
    echo "==> GIF assembled: $GIF"
  elif command -v ffmpeg >/dev/null 2>&1; then
    ffmpeg -y -framerate 1 -i "$FRAMES"/frame-%03d.png -vf "split[s0][s1];[s0]palettegen[p];[s1][p]paletteuse" "$GIF"
    echo "==> GIF assembled: $GIF"
  else
    echo "!!  neither ImageMagick nor ffmpeg found: frames are ready, assemble with:"
    echo "    magick -delay 60 -loop 0 $FRAMES/frame-*.png $OUT/kg-timelapse.gif"
  fi
fi

echo "==> done. Target: < 4 MB, legible at 1200 px wide (resize frames if needed)."
