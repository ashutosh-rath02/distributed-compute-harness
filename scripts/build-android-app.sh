#!/usr/bin/env bash
# Builds the Home Harness manager app for Android (roadmap item 9) —
# without Gradle, from the Android SDK's own tools, so it builds the same
# way on any machine with an SDK and a JDK (Windows Git Bash, macOS,
# Linux):
#
#   scripts/build-android-app.sh        -> dist/home-harness-manager.apk
#
# Needs: ANDROID_HOME (or ANDROID_SDK_ROOT) with a platform (android-33+)
# and build-tools; a JDK 11+ (javac, jar, keytool); Go.
#
# SIGNING: an update installs over the app only if it is signed with the
# same key — otherwise Android makes you uninstall it, which deletes the
# manager's state (its TLS certificate, which every agent has pinned).
# The key lives OUTSIDE the repo, created on first build:
#   $HARNESS_ANDROID_KEYSTORE   (default ~/.home-harness/android-signing.keystore)
#   and its password in the same path + ".pass"
# Back both up. To build on another machine, copy them there first.
#
# HARNESS_ANDROID_DEBUGGABLE=1 builds a debuggable variant (adb run-as can
# read the app's files) as dist/home-harness-manager-debug.apk — for
# emulator testing only; never install it on a phone you rely on.
set -euo pipefail
cd "$(dirname "$0")/.."

sdk="${ANDROID_HOME:-${ANDROID_SDK_ROOT:-}}"
[[ -n "$sdk" && -d "$sdk" ]] || { echo "set ANDROID_HOME to the Android SDK" >&2; exit 1; }
bt="$(ls -d "$sdk"/build-tools/* | sort -V | tail -1)"
platform="$(ls -d "$sdk"/platforms/android-* | sort -V | tail -1)/android.jar"
tool() { for c in "$bt/$1" "$bt/$1.exe" "$bt/$1.bat"; do [[ -e "$c" ]] && { echo "$c"; return; }; done; echo "missing $1 in $bt" >&2; exit 1; }
aapt2="$(tool aapt2)"; d8="$(tool d8)"; zipalign="$(tool zipalign)"; apksigner="$(tool apksigner)"

version_code="$(git rev-list --count HEAD 2>/dev/null || echo 1)"
version_name="$(git describe --always --dirty 2>/dev/null || echo dev)"
out="build/android"
rm -rf "$out"
mkdir -p "$out/assets/agents" "$out/lib" "$out/classes" "$out/gen" "$out/dex" dist

echo "== agent builds (handed out by the manager for onboarding and self-update)"
bash scripts/build-agents.sh >/dev/null
cp bin/agents/agent-* "$out/assets/agents/"

echo "== manager for each Android ABI (GOOS=linux: Android runs Linux binaries)"
build_manager() { # goos goarch goarm abi
  CGO_ENABLED=0 GOOS=$1 GOARCH=$2 GOARM=$3 go build -trimpath -ldflags "-s -w" -o "$out/lib/$4/libhomeharness_manager.so" ./cmd/manager
}
build_manager linux arm64 "" arm64-v8a
build_manager linux arm 7 armeabi-v7a
build_manager linux amd64 "" x86_64

echo "== resources"
"$aapt2" compile --dir android/res -o "$out/res.zip"
"$aapt2" link -o "$out/base.apk" -I "$platform" --manifest android/AndroidManifest.xml \
  -A "$out/assets" --java "$out/gen" --min-sdk-version 26 --target-sdk-version 36 \
  --version-code "$version_code" --version-name "$version_name" ${HARNESS_ANDROID_DEBUGGABLE:+--debug-mode} "$out/res.zip"

echo "== code"
javac -encoding UTF-8 --release 11 -nowarn -classpath "$platform" -d "$out/classes" \
  $(find android/src "$out/gen" -name '*.java')
(cd "$out/classes" && jar cf ../classes.jar .)
"$d8" --release --min-api 26 --lib "$platform" --output "$out/dex" "$out/classes.jar"

echo "== package and sign"
go run ./scripts/apkpack -in "$out/base.apk" -dex "$out/dex/classes.dex" -lib "$out/lib" -out "$out/unsigned.apk"
"$zipalign" -p -f 4 "$out/unsigned.apk" "$out/aligned.apk"
apk="dist/home-harness-manager${HARNESS_ANDROID_DEBUGGABLE:+-debug}.apk"
ks="${HARNESS_ANDROID_KEYSTORE:-$HOME/.home-harness/android-signing.keystore}"
if [[ ! -f "$ks" ]]; then
  mkdir -p "$(dirname "$ks")"
  od -An -N24 -tx1 /dev/urandom | tr -d ' \n' > "$ks.pass"
  chmod 600 "$ks.pass"
  keytool -genkeypair -keystore "$ks" -storepass "$(cat "$ks.pass")" -keypass "$(cat "$ks.pass")" -alias harness \
    -keyalg RSA -keysize 3072 -validity 10000 -dname "CN=Home Harness" >/dev/null 2>&1
  echo "!! created the app signing key $ks (+ .pass). BACK IT UP: without it, updates can't install over the app." >&2
fi
"$apksigner" sign --ks "$ks" --ks-pass "pass:$(cat "$ks.pass")" --ks-key-alias harness \
  --out "$apk" "$out/aligned.apk"
"$apksigner" verify "$apk"
echo "built $apk ($version_name, code $version_code)"
