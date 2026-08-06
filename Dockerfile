# Build stage: install libsrt dev, then build the Go relay
FROM golang:1.26 AS build

RUN apt-get update && apt-get install -y --no-install-recommends \
    cmake build-essential pkg-config git libssl-dev libzstd-dev \
    && rm -rf /var/lib/apt/lists/*

# Build libsrt from source (srtgo requires >= 1.4.2; 1.5.6 recommended for security).
WORKDIR /srt
RUN git clone --depth 1 --branch v1.5.6 https://github.com/Haivision/srt.git . \
    && mkdir -p build && cd build \
    && cmake -DCMAKE_BUILD_TYPE=Release -DENABLE_APPS=OFF -DENABLE_SHARED=ON -DENABLE_STATIC=OFF .. \
    && make -j$(nproc) && make install \
    && cd / && rm -rf /srt

WORKDIR /app
COPY go.mod go.sum* ./
COPY third_party/ third_party/
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 \
    CGO_CFLAGS="$(pkg-config --cflags libsrt)" \
    CGO_LDFLAGS="$(pkg-config --libs libsrt)" \
    go build -ldflags="-s -w" -o /srt-relay-app .

# Runtime: Debian slim + the libsrt shared lib (statically built, no apt needed at runtime)
FROM debian:trixie-slim
RUN apt-get update && apt-get install -y --no-install-recommends libssl3 libzstd1 zlib1g \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /usr/local/lib/libsrt.so* /usr/local/lib/
ENV LD_LIBRARY_PATH=/usr/local/lib
COPY --from=build /srt-relay-app /srt-relay-app
ENTRYPOINT ["/srt-relay-app"]
