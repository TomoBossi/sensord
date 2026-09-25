#!/data/data/com.termux/files/usr/bin/sh
# Builds build/sensord.apk without Gradle: go (c-shared) -> aapt2 -> javac -> d8 -> zip -> apksigner.
# Usage: ./build.sh [install]
set -eu
cd "$(dirname "$0")"

JAR=${ANDROID_JAR:-$HOME/.local/share/android-sdk/platforms/android-35/android.jar}
KS=${SENSORD_KEYSTORE:-$HOME/.local/share/android-keys/sensord.jks}
B=build

rm -rf $B && mkdir -p $B/lib/arm64-v8a $B/classes

CC="clang --target=aarch64-linux-android29" CGO_ENABLED=1 \
    go build -buildmode=c-shared -trimpath -ldflags=-s -o $B/lib/arm64-v8a/libsensord.so ./cmd/libsensord
rm -f $B/lib/arm64-v8a/libsensord.h

aapt2 link -o $B/base.apk -I "$JAR" --manifest app/AndroidManifest.xml
javac -Xlint:-options --release 11 -cp "$JAR" -d $B/classes $(find app/src -name '*.java')
d8 --release --min-api 29 --lib "$JAR" --output $B $(find $B/classes -name '*.class')

cp $B/base.apk $B/unsigned.apk
(cd $B && zip -qj unsigned.apk classes.dex && zip -qr unsigned.apk lib)

if [ ! -f "$KS" ]; then
    mkdir -p "$(dirname "$KS")"
    keytool -genkeypair -keystore "$KS" -storepass sensord -keypass sensord -alias sensord \
        -keyalg RSA -keysize 2048 -validity 36500 -dname CN=sensord >/dev/null 2>&1
fi
apksigner sign --ks "$KS" --ks-pass pass:sensord --out $B/sensord.apk $B/unsigned.apk
echo "built $B/sensord.apk"

if [ "${1:-}" = install ]; then
    adb install -r $B/sensord.apk
fi
