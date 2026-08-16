#!/bin/bash
#
# build-metallib.sh — compile MLX's Metal kernels into mlx.metallib.
#
# WHY THIS EXISTS
#
# SwiftPM (the `swift build` command line) has no build rule for `.metal`
# sources. mlx-swift says so itself:
#
#     "SwiftPM (command line) cannot build the Metal shaders so the ultimate
#      build has to be done via Xcode."   -- mlx-swift/README.md
#
# So `swift build -c release` links a complete binary whose Metal shader
# library was never compiled. The first MLX operation then dies with
#
#     MLX error: Failed to load the default metallib. library not found
#
# mlx's own CMake build produces exactly this artefact with `xcrun metal` /
# `xcrun metallib`; this script is that step, run against the mlx-swift
# checkout SwiftPM already resolved. See
# Source/Cmlx/mlx/mlx/backend/metal/kernels/CMakeLists.txt upstream — the
# flags, the include layout and the kernel list below all mirror it.
#
# WHERE THE OUTPUT GOES
#
# mlx looks for its shader library in this order (backend/metal/device.cpp):
#
#   1. <dir of the running binary>/mlx.metallib        <-- what we produce
#   2. <dir of the running binary>/Resources/mlx.metallib
#   3. <some bundle>/mlx-swift_Cmlx.bundle/default.metallib
#
# (1) is the cheapest and the least magical, and it is what mlx's CMake
# install emits, so the file must sit next to `kagaz-machelper-mlx`. Homebrew
# must therefore install both into the same directory.
#
# Only the kernels mlx pre-compiles under MLX_METAL_JIT are built here — the
# nine `.metal` files mlx-swift ships in Source/Cmlx/mlx-generated/metal, with
# their `#include`s already rewritten to be relative. Everything else mlx
# compiles at run time from the string preambles in mlx-generated/*.cpp.

set -euo pipefail

PACKAGE_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
CONFIGURATION=release
OUTPUT=""
MLX_SWIFT_DIR=${MLX_SWIFT_DIR:-}

usage() {
    cat <<'USAGE'
usage: Scripts/build-metallib.sh [--configuration release|debug] [--output PATH]

Compiles MLX's Metal kernels into mlx.metallib. With no --output the file is
written next to the built helper binary (.build/<configuration>/mlx.metallib),
which is where mlx looks for it first.

Environment:
  MLX_SWIFT_DIR   path to the resolved mlx-swift checkout; defaults to
                  .build/checkouts/mlx-swift (populated by `swift build`).
USAGE
}

while [ "$#" -gt 0 ]; do
    case "$1" in
        --configuration|-c)
            [ "$#" -ge 2 ] || { echo "build-metallib: $1 needs a value" >&2; exit 2; }
            CONFIGURATION=$2; shift 2 ;;
        --output|-o)
            [ "$#" -ge 2 ] || { echo "build-metallib: $1 needs a value" >&2; exit 2; }
            OUTPUT=$2; shift 2 ;;
        --help|-h)
            usage; exit 0 ;;
        *)
            echo "build-metallib: unknown argument $1" >&2; usage >&2; exit 2 ;;
    esac
done

case "$CONFIGURATION" in
    release|debug) ;;
    *) echo "build-metallib: --configuration must be release or debug" >&2; exit 2 ;;
esac

if [ -z "$OUTPUT" ]; then
    OUTPUT="${PACKAGE_DIR}/.build/${CONFIGURATION}/mlx.metallib"
fi

if [ -z "$MLX_SWIFT_DIR" ]; then
    MLX_SWIFT_DIR="${PACKAGE_DIR}/.build/checkouts/mlx-swift"
fi

KERNEL_DIR="${MLX_SWIFT_DIR}/Source/Cmlx/mlx-generated/metal"
if [ ! -d "$KERNEL_DIR" ]; then
    cat >&2 <<ERR
build-metallib: no mlx-swift checkout at ${MLX_SWIFT_DIR}

Run \`swift build -c ${CONFIGURATION}\` first (it resolves the package graph),
or point MLX_SWIFT_DIR at an existing checkout.
ERR
    exit 1
fi

command -v xcrun >/dev/null 2>&1 || {
    echo "build-metallib: xcrun not found; install the Xcode command line tools" >&2
    exit 1
}
xcrun -sdk macosx --find metal >/dev/null 2>&1 || {
    cat >&2 <<'ERR'
build-metallib: the Metal compiler is not available.

`xcrun -sdk macosx metal` is part of Xcode, not of the Command Line Tools. Install
Xcode and point at it, e.g.
  sudo xcode-select -s /Applications/Xcode.app/Contents/Developer
or run this script with DEVELOPER_DIR=/Applications/Xcode.app/Contents/Developer.
ERR
    exit 1
}

# Same nine kernels mlx's CMakeLists builds when MLX_METAL_JIT is on, which is
# the configuration mlx-swift ships (nojit_kernels.cpp is excluded from its
# SwiftPM target).
KERNELS=(
    arg_reduce
    conv
    gemv
    layer_norm
    random
    rms_norm
    rope
    scaled_dot_product_attention
    steel/attn/kernels/steel_attention
)

# Deployment target must match the package's: MLX picks a Metal language
# version from the OS at run time and macOS 15 is the floor everywhere in Kagaz.
DEPLOYMENT_TARGET=15.0

# metal_3_1 is the >= Metal 3.1 bf16 header set, which every macOS 15 device has.
METAL_FLAGS=(
    -Wall -Wextra -fno-fast-math -Wno-c++17-extensions
    "-mmacosx-version-min=${DEPLOYMENT_TARGET}"
    -I"${KERNEL_DIR}"
    -I"${KERNEL_DIR}/metal_3_1"
)

WORK=$(mktemp -d "${TMPDIR:-/tmp}/kagaz-metallib.XXXXXX")
trap 'rm -rf "${WORK}"' EXIT

AIR=()
for kernel in "${KERNELS[@]}"; do
    source_file="${KERNEL_DIR}/${kernel}.metal"
    [ -f "$source_file" ] || {
        echo "build-metallib: missing kernel source ${source_file}" >&2
        echo "build-metallib: the resolved mlx-swift may not match this script; see Package.resolved" >&2
        exit 1
    }
    air="${WORK}/$(basename "${kernel}").air"
    echo "  metal ${kernel}.metal"
    xcrun -sdk macosx metal "${METAL_FLAGS[@]}" -c "$source_file" -o "$air"
    AIR+=("$air")
done

mkdir -p "$(dirname "$OUTPUT")"
xcrun -sdk macosx metallib "${AIR[@]}" -o "$OUTPUT"
echo "build-metallib: wrote ${OUTPUT}"
