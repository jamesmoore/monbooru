#!/bin/sh
# packaging/fetch-tools.sh
#
# Downloads the bundled tools for one target into tools/
set -eu

ort_arch=""
ff_arch=""
ort_sha=""
ff_sha=""
dml_dir=""
case "${GOOS}/${GOARCH}" in
  linux/amd64)   ort_arch=x64;      ff_arch=linux64;    ort_sha=$ORT_SHA256_LINUX_X64;     ff_sha=$FFMPEG_SHA256_LINUX64 ;;
  linux/arm64)   ort_arch=aarch64;  ff_arch=linuxarm64; ort_sha=$ORT_SHA256_LINUX_AARCH64; ff_sha=$FFMPEG_SHA256_LINUXARM64 ;;
  windows/amd64) ort_arch=win-x64;  ff_arch=win64;      ort_sha=$ORT_DML_SHA256_WIN_X64;   ff_sha=$FFMPEG_SHA256_WIN64 ;;
  *) echo "no bundled tools published for ${GOOS}/${GOARCH}" >&2; exit 1 ;;
esac

verify() {
  echo "$2  $1" | sha256sum -c -
}

mkdir -p tools
ort_base="https://github.com/microsoft/onnxruntime/releases/download/v${ORT_VERSION}"
ff_base="https://github.com/monbooru/ffmpeg-builds/releases/download/${FFMPEG_BUILD}"

if [ "$GOOS" = windows ]; then
  rm -rf /tmp/ort /tmp/dml /tmp/ff
  curl -fsSL -o /tmp/ort.zip "${ort_base}/Microsoft.ML.OnnxRuntime.DirectML.${ORT_VERSION}.nupkg"
  verify /tmp/ort.zip "$ort_sha"
  unzip -q /tmp/ort.zip -d /tmp/ort
  ort_dir=/tmp/ort
  cp "$ort_dir/runtimes/${ort_arch}/native/onnxruntime.dll" tools/
  dml_base="https://api.nuget.org/v3-flatcontainer/microsoft.ai.directml/${DIRECTML_VERSION}"
  curl -fsSL -o /tmp/dml.zip "${dml_base}/microsoft.ai.directml.${DIRECTML_VERSION}.nupkg"
  verify /tmp/dml.zip "$DIRECTML_SHA256"
  unzip -q /tmp/dml.zip -d /tmp/dml
  dml_dir=/tmp/dml
  cp "$dml_dir/bin/x64-win/DirectML.dll" tools/
  curl -fsSL -o /tmp/ffmpeg.zip "${ff_base}/${FFMPEG_NAME}-${ff_arch}.zip"
  verify /tmp/ffmpeg.zip "$ff_sha"
  unzip -q /tmp/ffmpeg.zip -d /tmp/ff
  ff_dir="/tmp/ff/${FFMPEG_NAME}-${ff_arch}"
  cp "$ff_dir/bin/ffmpeg.exe" "$ff_dir/bin/ffprobe.exe" tools/
else
  curl -fsSL -o /tmp/ort.tgz "${ort_base}/onnxruntime-linux-${ort_arch}-${ORT_VERSION}.tgz"
  verify /tmp/ort.tgz "$ort_sha"
  tar -xzf /tmp/ort.tgz -C /tmp
  ort_dir="/tmp/onnxruntime-linux-${ort_arch}-${ORT_VERSION}"
  cp "$ort_dir/lib/libonnxruntime.so.${ORT_VERSION}" tools/libonnxruntime.so
  curl -fsSL -o /tmp/ffmpeg.tar.xz "${ff_base}/${FFMPEG_NAME}-${ff_arch}.tar.xz"
  verify /tmp/ffmpeg.tar.xz "$ff_sha"
  tar -xJf /tmp/ffmpeg.tar.xz -C /tmp
  ff_dir="/tmp/${FFMPEG_NAME}-${ff_arch}"
  cp "$ff_dir/bin/ffmpeg" "$ff_dir/bin/ffprobe" tools/
fi

mkdir -p tools/licenses/onnxruntime tools/licenses/ffmpeg
cp "$ort_dir/LICENSE" "$ort_dir/ThirdPartyNotices.txt" tools/licenses/onnxruntime/
cp "$ff_dir"/licenses/* tools/licenses/ffmpeg/
if [ -n "$dml_dir" ]; then
  mkdir -p tools/licenses/directml
  cp "$dml_dir/LICENSE.txt" "$dml_dir/LICENSE-CODE.txt" "$dml_dir/ThirdPartyNotices.txt" tools/licenses/directml/
fi

ls -la tools
