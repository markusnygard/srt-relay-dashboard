# Building

The relay links against the native SRT library (`libsrt`), so it must be built
in a Linux environment with SRT 1.5.6+ installed. The easiest way is the
included Dockerfile, which compiles libsrt from source and produces a binary
plus the shared library you copy to the target server.

## Prerequisites

- Docker (BuildKit recommended)

## Build

```bash
docker build -t srt-relay-dashboard .
```

## Extract artifacts for a bare-metal deployment

```bash
# create a throwaway container so we can copy files out
docker create --name relay-extract srt-relay-dashboard

docker cp relay-extract:/srt-relay-app ./
docker cp relay-extract:/usr/local/lib/libsrt.so.1.5.6 ./

docker rm relay-extract
```

You now have:

- `srt-relay-app` — the Go relay + web dashboard binary
- `libsrt.so.1.5.6` — the SRT shared library it needs at runtime

## Deploy on the server

```bash
# same directory as the binary
export LD_LIBRARY_PATH=$(pwd)

# verify the library is found
ldd ./srt-relay-app

# run it
./srt-relay-app --http 0.0.0.0:8080
```

## Building locally without Docker (Linux only)

```bash
sudo apt install -y cmake build-essential pkg-config libssl-dev libzstd-dev

# build libsrt 1.5.6 from source
git clone --depth 1 --branch v1.5.6 https://github.com/Haivision/srt.git
cd srt && mkdir build && cd build
cmake -DCMAKE_BUILD_TYPE=Release -DENABLE_APPS=OFF -DENABLE_SHARED=ON -DENABLE_STATIC=OFF ..
make -j$(nproc) && sudo make install

# build the app
cd ../../
export CGO_ENABLED=1
go build -o srt-relay-app .
```

## Why a shared libsrt?

`srtgo` (the Go SRT binding) is cgo and links `-lsrt`. Fully static linking is
possible but pulls in glibc/OpenSSL static deps and is fragile; shipping the
single `libsrt.so` beside the binary is simpler and equally reliable on a
fixed server image (the server runs Ubuntu 24.04).
